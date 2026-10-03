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
	defer resp.Body.Close()
	c.noteRateLimit(resp)
	if resp.StatusCode/100 != 2 {
		if isRateLimited(resp) {
			return c.rateLimitError(resp)
		}
		return ghHTTPError(method, url, resp)
	}
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
		switch e.Type {
		case "NOT_FOUND":
			return plugin.Fail(plugin.CodeTargetGone, msg, nil)
		case "FORBIDDEN":
			return plugin.Fail(plugin.CodeUpstream, msg, map[string]any{"status": http.StatusForbidden, "retryable": false})
		case "RATE_LIMITED":
			return plugin.Fail(plugin.CodeRateLimited, msg, nil)
		default:
			return plugin.Fail(plugin.CodeInvalid, msg, nil)
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
	defer resp.Body.Close()
	c.noteRateLimit(resp)
	if resp.StatusCode/100 != 2 {
		if isRateLimited(resp) {
			return c.rateLimitError(resp)
		}
		return ghHTTPError("POST", url, resp)
	}
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
// decision that must see state as of now (review state before a re-request),
// where a cached body up to CacheTTL old could be the wrong answer.
func (c *Client) getFresh(ctx context.Context, token, url string, out any) error {
	resp, err := c.getRaw(ctx, token, url, "application/vnd.github+json", "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.noteRateLimit(resp)
	if resp.StatusCode/100 != 2 {
		if isRateLimited(resp) {
			return c.rateLimitError(resp)
		}
		return ghHTTPError("GET", url, resp)
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

		case isRateLimited(resp):
			wait := retryAfter(resp)
			resp.Body.Close()
			if attempt == 0 && wait > 0 && wait <= maxRateWait {
				if err := sleepCtx(ctx, wait); err != nil {
					return nil, err
				}
				continue // one retry after the window
			}
			// Prefer stale data over failing the caller — a review shouldn't
			// die because the limit blipped when we already hold the diff.
			c.mu.Lock()
			if e := c.getCache[key]; e != nil {
				body := e.body
				c.mu.Unlock()
				return body, nil
			}
			c.mu.Unlock()
			return nil, c.rateLimitError(resp)

		default:
			err := ghHTTPError("GET", url, resp)
			resp.Body.Close()
			return nil, err
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

// rateLimitError builds the contract's rate_limited answer (§1.11):
// data.retry_after is a Go duration string, taken from resp's own
// Retry-After or X-RateLimit-Reset (retryAfter — covers both the secondary
// and primary limit signals), falling back to the client's last-noted reset
// when resp carries neither.
func (c *Client) rateLimitError(resp *http.Response) error {
	wait := retryAfter(resp)
	if wait <= 0 {
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
// from an already-resolved wait, for a caller with no client-level
// rate-limit cache to fall back on (the App-auth token endpoints, which
// share no state with a Client's GET cache).
func rateLimitErrorForWait(wait time.Duration) error {
	msg := "github: rate limit reached"
	data := map[string]any{}
	if wait > 0 {
		wait = wait.Round(time.Second)
		msg = fmt.Sprintf("github: rate limit reached; resets in %s", wait)
		data["retry_after"] = wait.String()
	}
	return plugin.Fail(plugin.CodeRateLimited, msg, data)
}

// IsRateLimited reports whether a response is a GitHub rate-limit refusal —
// primary (403 with X-RateLimit-Remaining: 0) or secondary (403/429 with a
// Retry-After).
func IsRateLimited(resp *http.Response) bool { return isRateLimited(resp) }

func isRateLimited(resp *http.Response) bool {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	return resp.Header.Get("X-RateLimit-Remaining") == "0"
}

// RetryAfter is how long to wait before retrying a rate-limited response,
// from Retry-After (seconds) or the X-RateLimit-Reset epoch, clamped to a
// sane bound.
func RetryAfter(resp *http.Response) time.Duration { return retryAfter(resp) }

func retryAfter(resp *http.Response) time.Duration {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
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

// ghHTTPError renders a non-2xx, non-rate-limited GitHub response into the
// plugin contract's error (§1.11), surfacing the API's own message when
// present. isRateLimited(resp) must already have been checked false by the
// caller — a rate limit answers rate_limited (rateLimitError), never this.
func ghHTTPError(method, url string, resp *http.Response) error {
	var msg struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&msg)
	text := fmt.Sprintf("%s %s: HTTP %d", method, url, resp.StatusCode)
	if msg.Message != "" {
		text += ": " + msg.Message
	}
	return contractHTTPError(resp.StatusCode, text)
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
