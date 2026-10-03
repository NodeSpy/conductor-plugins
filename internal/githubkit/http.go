package githubkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

const (
	// maxReadBytes caps a raw text read (a diff, a repo file) so a
	// pathologically large response can't exhaust memory. A body over the
	// limit is truncated.
	maxReadBytes = 16 << 20 // 16 MiB
	// maxCacheEntries bounds the read cache over the client's lifetime.
	maxCacheEntries = 1024
	// maxRateWait caps how long a single GET will block waiting out a rate
	// limit before giving up (and serving stale, or erroring).
	maxRateWait = 30 * time.Second
)

// post/patch/put/del are the write verbs' authenticated JSON requests.
func (c *Client) post(ctx context.Context, token, url string, body, out any) error {
	return c.send(ctx, http.MethodPost, token, url, body, out)
}
func (c *Client) patch(ctx context.Context, token, url string, body, out any) error {
	return c.send(ctx, http.MethodPatch, token, url, body, out)
}
func (c *Client) put(ctx context.Context, token, url string, body, out any) error {
	return c.send(ctx, http.MethodPut, token, url, body, out)
}
func (c *Client) del(ctx context.Context, token, url string, body any) error {
	return c.send(ctx, http.MethodDelete, token, url, body, nil)
}

// send issues one authenticated JSON request of any method. A nil body sends
// no content. Any successful write invalidates the read cache, so a
// mutate-then-read (e.g. merge_pr then pr_get) never serves the pre-write
// copy.
func (c *Client) send(ctx context.Context, method, token, url string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	c.noteRateLimit(resp)
	if resp.StatusCode/100 != 2 {
		return c.httpFailure(method, url, resp)
	}
	defer resp.Body.Close()
	c.invalidateCache() // a write may have changed what a cached read returns
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// invalidateCache drops every cached GET body (called after a successful write).
func (c *Client) invalidateCache() {
	c.mu.Lock()
	c.getCache = map[string]*cacheEntry{}
	c.mu.Unlock()
}

// graphql runs one GraphQL (v4) query/mutation. GraphQL returns 200 even on
// query errors, so those are surfaced from the response body, not the status.
func (c *Client) graphql(ctx context.Context, token, query string, variables map[string]any, out any) error {
	reqBody := map[string]any{"query": query}
	if len(variables) > 0 {
		reqBody["variables"] = variables
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}
	if err := c.post(ctx, token, c.base()+"/graphql", reqBody, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		e := resp.Errors[0]
		msg := fmt.Sprintf("github graphql: %s", e.Message)
		// GraphQL answers 200 even on a query error (§1.11 has no HTTP status
		// to classify from here): GitHub's own error "type" extension is the
		// only signal. NOT_FOUND is the node the mutation named going away
		// (target_gone); FORBIDDEN is a permission answer (upstream, not
		// retried); anything else is the request itself being malformed
		// (invalid) — our own hand-written queries, so in practice a bug, but
		// never worth retrying either way.
		//
		// Only a few "type" values are documented
		// (https://docs.github.com/en/graphql/overview/handling-errors):
		// NOT_FOUND is classified the same as a REST 404 (status 404, not
		// target_gone directly) so it flows through the exact same
		// target-keyed remap every other verb's 404 does
		// (remapTargetGone/remapGoneIfMissing, including the no-access-vs-
		// gone repo check) — a call site that doesn't address an event's own
		// target (reactReview's review id, say) never remaps it either, same
		// as a REST sub-resource 404. FORBIDDEN is a permission answer
		// (upstream, never retried). RATE_LIMITED is rate_limited.
		// UNPROCESSABLE is GitHub's own "this input can't be processed" —
		// the one type that names OUR hand-written query/mutation as the
		// problem, never worth retrying. Any other type, or none at all (a
		// transient INTERNAL/SERVICE_UNAVAILABLE, or a type GitHub adds
		// later), is left retryable rather than guessed invalid: an
		// unrecognized type is far more likely to be transient than a bug in
		// a query that otherwise runs fine.
		switch e.Type {
		case "NOT_FOUND":
			return plugin.Fail(plugin.CodeUpstream, msg, map[string]any{"status": http.StatusNotFound, "retryable": false})
		case "FORBIDDEN":
			return plugin.Fail(plugin.CodeUpstream, msg, map[string]any{"status": http.StatusForbidden, "retryable": false})
		case "RATE_LIMITED":
			return plugin.Fail(plugin.CodeRateLimited, msg, map[string]any{"retry_after": defaultRateLimitWait.String()})
		case "UNPROCESSABLE":
			return plugin.Fail(plugin.CodeInvalid, msg, nil)
		default:
			return plugin.Fail(plugin.CodeUpstream, msg, map[string]any{"retryable": true})
		}
	}
	if out != nil && len(resp.Data) > 0 {
		return json.Unmarshal(resp.Data, out)
	}
	return nil
}

// postRaw uploads a raw body (a release asset) with a caller-set content type
// — release assets go to a separate uploads host, so the caller passes the
// full upload URL. Invalidates the read cache like any write.
func (c *Client) postRaw(ctx context.Context, token, url, contentType string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	c.noteRateLimit(resp)
	if resp.StatusCode/100 != 2 {
		return c.httpFailure("POST", url, resp)
	}
	defer resp.Body.Close()
	c.invalidateCache()
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// get issues an authenticated JSON GET (cached) and decodes into out.
func (c *Client) get(ctx context.Context, token, url string, out any) error {
	b, err := c.cachedGet(ctx, token, url, "application/vnd.github+json")
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// getFresh issues an authenticated GET that bypasses the read cache — for a
// decision that must see state as of now (review state before a re-request,
// repoVisible's visibility probe), where a cached body up to CacheTTL old
// could be the wrong answer. A nil out (repoVisible cares only whether the
// GET succeeded, not the body) drains and discards the body instead of
// decoding it — json.Decode(nil) would otherwise error even on a 2xx.
func (c *Client) getFresh(ctx context.Context, token, url string, out any) error {
	resp, err := c.getRaw(ctx, token, url, "application/vnd.github+json", "")
	if err != nil {
		return err
	}
	c.noteRateLimit(resp)
	if resp.StatusCode/100 != 2 {
		return c.httpFailure("GET", url, resp)
	}
	defer resp.Body.Close()
	if out == nil {
		_, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxReadBytes))
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxReadBytes)).Decode(out)
}

// getText issues an authenticated GET (cached) with a caller-supplied Accept
// (the diff or raw media type) and returns the body as a string.
func (c *Client) getText(ctx context.Context, token, url, accept string) (string, error) {
	b, err := c.cachedGet(ctx, token, url, accept)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// cachedGet is the read path shared by every GET verb: an in-process TTL
// cache with ETag revalidation, and rate-limit awareness. A fresh entry is
// served without a round-trip; a stale one revalidates conditionally (a 304
// costs no body). On a rate-limit response it waits out a short Retry-After
// once, then falls back to a stale cached body if it has one, else errors
// with the reset.
func (c *Client) cachedGet(ctx context.Context, token, url, accept string) ([]byte, error) {
	key := cacheKey(token, accept, url)
	now := time.Now()

	c.mu.Lock()
	e := c.getCache[key]
	if e != nil && now.Sub(e.fetched) < c.CacheTTL {
		body := e.body
		c.mu.Unlock()
		return body, nil
	}
	etag := ""
	if e != nil {
		etag = e.etag
	}
	c.mu.Unlock()

	for attempt := 0; ; attempt++ {
		resp, err := c.getRaw(ctx, token, url, accept, etag)
		if err != nil {
			return nil, err
		}
		c.noteRateLimit(resp)

		switch {
		case resp.StatusCode == http.StatusNotModified:
			resp.Body.Close()
			c.mu.Lock()
			if e := c.getCache[key]; e != nil {
				e.fetched = time.Now()
				body := e.body
				c.mu.Unlock()
				return body, nil
			}
			c.mu.Unlock()
			etag = "" // cache was evicted under us — refetch unconditionally
			continue

		case resp.StatusCode/100 == 2:
			b, rerr := io.ReadAll(io.LimitReader(resp.Body, maxReadBytes))
			newEtag := resp.Header.Get("ETag")
			resp.Body.Close()
			if rerr != nil {
				return nil, rerr
			}
			c.storeCache(key, newEtag, b)
			return b, nil

		default:
			msg := readBody(resp)
			if isRateLimited(resp, msg) {
				wait := retryAfter(resp, msg)
				if attempt == 0 && wait > 0 && wait <= maxRateWait {
					if err := sleepCtx(ctx, wait); err != nil {
						return nil, err
					}
					continue // one retry after the window
				}
				// Prefer stale data over failing the caller — a review
				// shouldn't die because the limit blipped when we already
				// hold the diff.
				c.mu.Lock()
				if e := c.getCache[key]; e != nil {
					body := e.body
					c.mu.Unlock()
					return body, nil
				}
				c.mu.Unlock()
				return nil, c.rateLimitError(resp, msg)
			}
			return nil, ghHTTPError("GET", url, resp.StatusCode, msg)
		}
	}
}

// getRaw builds and sends an authenticated GET; the caller reads/closes the
// body. A non-empty etag makes it a conditional request (If-None-Match).
func (c *Client) getRaw(ctx context.Context, token, url, accept, etag string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	return c.httpc.Do(req)
}

func (c *Client) storeCache(key, etag string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.getCache) >= maxCacheEntries {
		now := time.Now()
		for k, e := range c.getCache { // drop expired first
			if now.Sub(e.fetched) >= c.CacheTTL {
				delete(c.getCache, k)
			}
		}
		if len(c.getCache) >= maxCacheEntries {
			c.getCache = map[string]*cacheEntry{} // still full: reset
		}
	}
	c.getCache[key] = &cacheEntry{etag: etag, body: body, fetched: time.Now()}
}

// noteRateLimit records the primary rate-limit state from a response's headers.
func (c *Client) noteRateLimit(resp *http.Response) {
	rem, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
	if err != nil {
		return
	}
	c.mu.Lock()
	c.rlRemaining = rem
	if s, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		c.rlReset = time.Unix(s, 0)
	}
	c.mu.Unlock()
}

// defaultRateLimitWait is the retry_after reported when a rate-limited
// answer gives no duration to compute one from — chiefly GitHub's secondary
// rate limit / abuse-detection refusal, which (unlike the primary limit)
// sends neither Retry-After nor X-RateLimit-Remaining:0, so there is no
// header to read at all; also used for the GraphQL RATE_LIMITED error type,
// which (answering 200) carries no headers of any kind.
const defaultRateLimitWait = 60 * time.Second

// rateLimitError builds the contract's rate_limited answer (§1.11):
// data.retry_after is a Go duration string, taken from resp's own
// Retry-After header, else — for a PRIMARY exhaustion only —
// X-RateLimit-Reset (retryAfter); falling back to the client's last-noted
// primary reset, then to defaultRateLimitWait, when resp carries neither.
// The client's last-noted reset is itself a PRIMARY-budget signal (set by
// noteRateLimit), so it is skipped for a secondary limit exactly like
// X-RateLimit-Reset is — using it here would quietly reintroduce the same
// wrong-reset bug through the fallback path.
func (c *Client) rateLimitError(resp *http.Response, msg string) error {
	wait := retryAfter(resp, msg)
	if wait <= 0 && !isSecondaryRateLimit(resp, msg) {
		c.mu.Lock()
		reset := c.rlReset
		c.mu.Unlock()
		if !reset.IsZero() {
			wait = time.Until(reset)
		}
	}
	return rateLimitErrorForWait(wait)
}

// rateLimitErrorForWait builds the contract's rate_limited answer (§1.11)
// from an already-resolved wait — defaultRateLimitWait when it is zero or
// negative (no header gave one) — for a caller with no client-level
// rate-limit cache to fall back on (the App-auth token endpoints, which
// share no state with a Client's GET cache).
func rateLimitErrorForWait(wait time.Duration) error {
	if wait <= 0 {
		wait = defaultRateLimitWait
	}
	wait = wait.Round(time.Second)
	msg := fmt.Sprintf("github: rate limit reached; resets in %s", wait)
	return plugin.Fail(plugin.CodeRateLimited, msg, map[string]any{"retry_after": wait.String()})
}

// looksLikeSecondaryRateLimit reports whether an error message is GitHub's
// secondary rate limit / abuse-detection refusal — a 403 (sometimes 429)
// that carries neither Retry-After nor X-RateLimit-Remaining:0, so the
// message (or, in the same spirit, the response's documentation_url) is the
// only signal. Matched against GitHub's actual wording for the two refusals
// ("You have exceeded a secondary rate limit...", "...abuse detection
// mechanism...") and the documentation_url slug they both link
// (#abuse-rate-limits/#secondary-rate-limits) — NOT a bare "abuse" substring,
// which would also match an ordinary 403 whose message happens to use that
// word in an unrelated sense (content moderation, a report of abuse, etc.)
// and misclassify it as rate_limited.
func looksLikeSecondaryRateLimit(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "secondary rate limit") ||
		strings.Contains(lower, "abuse detection mechanism") ||
		strings.Contains(lower, "abuse-rate-limits") ||
		strings.Contains(lower, "secondary-rate-limits")
}

// IsRateLimited reports whether a response is a GitHub rate-limit refusal —
// primary (403 with X-RateLimit-Remaining: 0) or secondary (403/429 with a
// Retry-After). A caller with only the *http.Response (its body already
// consumed or never read) can't see the message-only secondary-limit case;
// isRateLimited (internal, used on the read path while the body is still
// available) covers that one too.
func IsRateLimited(resp *http.Response) bool { return isRateLimited(resp, "") }

// isRateLimited is IsRateLimited plus the message-only secondary rate limit
// / abuse-detection case (msg is the body's own "message" field, read once
// by readBody — see looksLikeSecondaryRateLimit).
func isRateLimited(resp *http.Response, msg string) bool {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return true
	}
	return looksLikeSecondaryRateLimit(msg)
}

// RetryAfter is how long to wait before retrying a rate-limited response,
// from Retry-After (seconds, or an HTTP-date) or — for a primary exhaustion
// only — the X-RateLimit-Reset epoch, clamped to a sane bound. A caller with
// only the *http.Response (its body already consumed or never read) can't
// see the message-only secondary-limit case when X-RateLimit-Remaining is
// also absent; retryAfter (internal, used on the read path while the body is
// still available) covers that one too — see isSecondaryRateLimit.
func RetryAfter(resp *http.Response) time.Duration { return retryAfter(resp, "") }

// retryAfter is RetryAfter plus the message-only secondary-limit case (msg
// is the body's own "message" field, read once by readBody).
//
// Retry-After always wins when present — it's GitHub's own answer to "how
// long", for either limit. Absent that, X-RateLimit-Reset is trustworthy
// ONLY for a PRIMARY exhaustion: it is the primary budget's refill time, and
// a secondary rate limit / abuse-detection refusal can fire with the
// primary budget nowhere near exhausted (X-RateLimit-Remaining still
// positive — or the header missing outright on some secondary-limit
// responses) and a Reset that is simply unrelated to when the secondary
// limit itself clears, sometimes tens of minutes away. Reading it anyway
// turns a 60-second secondary cooldown into a wait tens of minutes too long.
// isSecondaryRateLimit tells the two apart; a secondary limit with no
// Retry-After falls through to the caller's defaultRateLimitWait instead.
func retryAfter(resp *http.Response, msg string) time.Duration {
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
		// Retry-After may also be an HTTP-date (RFC 7231 §7.1.3) rather than
		// a number of seconds.
		if t, err := http.ParseTime(ra); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
			return 0
		}
	}
	if isSecondaryRateLimit(resp, msg) {
		return 0 // X-RateLimit-Reset describes the (unexhausted) primary budget — meaningless here
	}
	if rs := resp.Header.Get("X-RateLimit-Reset"); rs != "" {
		if s, err := strconv.ParseInt(rs, 10, 64); err == nil {
			if d := time.Until(time.Unix(s, 0)); d > 0 {
				return d
			}
		}
	}
	return 0
}

// isSecondaryRateLimit reports whether a rate-limited response (isRateLimited
// already true) is GitHub's secondary limit rather than a primary
// exhaustion: X-RateLimit-Remaining present and nonzero means the primary
// budget is untouched, so whatever triggered the refusal must be the
// secondary one. When that header is absent entirely (some secondary-limit
// responses carry no rate-limit headers at all), fall back to GitHub's own
// message/documentation_url wording.
func isSecondaryRateLimit(resp *http.Response, msg string) bool {
	if rem := resp.Header.Get("X-RateLimit-Remaining"); rem != "" {
		return rem != "0"
	}
	return looksLikeSecondaryRateLimit(msg)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cacheKey namespaces a cached GET by token (so as:me and as:bot never share)
// without storing the secret, plus the Accept (diff vs json vs raw differ)
// and the URL.
func cacheKey(token, accept, url string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(token))
	return strconv.FormatUint(h.Sum64(), 36) + "\x00" + accept + "\x00" + url
}

// readBody reads and closes resp.Body (capped at maxReadBytes) and extracts
// the API's own "message" field, when the body is the usual
// {"message": "..."} error envelope. Reading the body ONCE, before deciding
// whether a non-2xx response is rate_limited, is what lets the secondary
// rate limit / abuse-detection case be recognized at all — it carries no
// header, only this message.
func readBody(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxReadBytes))
	resp.Body.Close()
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Message
}

// httpFailure classifies a non-2xx response into the plugin contract's
// error codes (§1.11), reading the body once so both the rate-limit check
// (isRateLimited's secondary-limit detection is message-only) and the
// generic classifier (ghHTTPError) see GitHub's own message. Closes
// resp.Body; the caller must not also close or read it.
func (c *Client) httpFailure(method, url string, resp *http.Response) error {
	msg := readBody(resp)
	if isRateLimited(resp, msg) {
		return c.rateLimitError(resp, msg)
	}
	return ghHTTPError(method, url, resp.StatusCode, msg)
}

// ghHTTPError renders a non-2xx, non-rate-limited GitHub response (status,
// and its own "message" field when present) into the plugin contract's
// error (§1.11). isRateLimited must already have been checked false by the
// caller — a rate limit answers rate_limited (rateLimitError), never this.
func ghHTTPError(method, url string, status int, msg string) error {
	text := fmt.Sprintf("%s %s: HTTP %d", method, url, status)
	if msg != "" {
		text += ": " + msg
	}
	return contractHTTPError(status, text)
}

// contractHTTPError classifies a non-2xx, non-rate-limited status into the
// plugin contract's error codes (§1.11): 422 is invalid (never retried — the
// request can't succeed as shaped), anything else is upstream, retryable
// only for a 5xx ("other 4xx" is not retried). target_gone is a caller remap
// (remapTargetGone, remapGoneIfMissing) applied only at call sites whose URL
// addresses a PR/issue directly — this function has no way to know that.
func contractHTTPError(status int, msg string) error {
	if status == http.StatusUnprocessableEntity {
		return plugin.Fail(plugin.CodeInvalid, msg, map[string]any{"status": status})
	}
	return plugin.Fail(plugin.CodeUpstream, msg, map[string]any{
		"status":    status,
		"retryable": status/100 == 5,
	})
}
