package ghfake

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mrand "math/rand"
	"net/http"
	"strings"
	"time"
)

// Delivery is one webhook delivery attempt.
type Delivery struct {
	GUID    string
	Event   string // X-GitHub-Event
	Action  string
	Repo    string
	HookID  int64
	URL     string
	Body    []byte
	secret  string
	Status  int // the receiver's HTTP status (0: not delivered yet / transport error)
	Err     string
	Attempt int
}

// Payload decodes the delivery body.
func (d *Delivery) Payload() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(d.Body, &m)
	return m
}

// WebhookName is the OpenAPI webhook name ("pull-request-review-submitted")
// of an event and action.
func WebhookName(event, action string) string {
	n := strings.ReplaceAll(event, "_", "-")
	if action != "" {
		n += "-" + strings.ReplaceAll(action, "_", "-")
	}
	return n
}

func guid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// prepare builds the deliveries one model change produces — one per matching
// hook — without queueing them.
func (f *Fake) prepare(r *Repo, event, action string, sender *User, extra map[string]any) []*Delivery {
	payload := map[string]any{"repository": f.repoJSON(r), "sender": f.userJSON(sender)}
	if action != "" {
		payload["action"] = action
	}
	if r.InstallationID != 0 {
		payload["installation"] = map[string]any{"id": r.InstallationID, "node_id": nodeID("MDIz", r.InstallationID)}
	}
	if r.Owner.Type == "Organization" {
		payload["organization"] = ShapeOf("organization-simple", map[string]any{"login": r.Owner.Login, "id": r.Owner.ID, "node_id": nodeID("O", r.Owner.ID)})
	}
	for k, v := range extra {
		payload[k] = v
	}
	name := WebhookName(event, action)
	if schema, ok := WebhookSchema(name); ok {
		payload = Shape(schema, payload)
	} else {
		f.opts.Logf("ghfake: webhook %s has no vendored schema — sent unshaped", name)
	}
	body, _ := json.Marshal(payload)
	var out []*Delivery
	for _, h := range f.hooks {
		if !hookWants(h, r, event) {
			continue
		}
		out = append(out, &Delivery{GUID: guid(), Event: event, Action: action, Repo: r.FullName(), HookID: h.ID, URL: h.URL, Body: body, secret: h.Secret})
	}
	return out
}

func hookWants(h *Hook, r *Repo, event string) bool {
	if len(h.Repos) > 0 && !contains(h.Repos, lower(r.FullName())) {
		return false
	}
	return len(h.Events) == 0 || contains(h.Events, event) || contains(h.Events, "*")
}

// emit prepares and queues one change's deliveries.
func (f *Fake) emit(r *Repo, event, action string, sender *User, extra map[string]any) {
	f.enqueue(f.prepare(r, event, action, sender, extra)...)
}

// order arranges a submitted review's deliveries per Options.ReviewOrder.
func (f *Fake) order(review, comments []*Delivery) []*Delivery {
	switch f.opts.ReviewOrder {
	case CommentsFirst:
		return append(append([]*Delivery(nil), comments...), review...)
	case Shuffled:
		all := append(append([]*Delivery(nil), review...), comments...)
		rnd := mrand.New(mrand.NewSource(f.opts.Seed + int64(len(f.deliveries))))
		rnd.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
		return all
	}
	return append(append([]*Delivery(nil), review...), comments...)
}

func (f *Fake) enqueue(ds ...*Delivery) {
	if f.closed {
		return
	}
	for _, d := range ds {
		n := 1
		if f.opts.DuplicateDeliveries {
			n = 2
		}
		for i := 0; i < n; i++ {
			dd := *d
			dd.Attempt = i + 1
			f.deliveries = append(f.deliveries, &dd)
			f.pending.Add(1)
			f.queue <- &dd
		}
	}
}

// Redeliver re-sends a delivery with the SAME GUID — what GitHub's
// "Redeliver" does.
func (f *Fake) Redeliver(guid string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.deliveries) - 1; i >= 0; i-- {
		if d := f.deliveries[i]; d.GUID == guid {
			dd := *d
			dd.Attempt, dd.Status, dd.Err = d.Attempt+1, 0, ""
			f.deliveries = append(f.deliveries, &dd)
			f.pending.Add(1)
			f.queue <- &dd
			return true
		}
	}
	return false
}

// Flush blocks until every queued delivery has been attempted.
func (f *Fake) Flush() { f.pending.Wait() }

// Deliveries lists every delivery attempt so far.
func (f *Fake) Deliveries() []Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Delivery, 0, len(f.deliveries))
	for _, d := range f.deliveries {
		out = append(out, *d)
	}
	return out
}

var hookClient = &http.Client{Timeout: 15 * time.Second}

func (f *Fake) deliverLoop() {
	for d := range f.queue {
		code, err := send(d)
		f.mu.Lock()
		d.Status = code
		if err != nil {
			d.Err = err.Error()
		}
		f.mu.Unlock()
		if err != nil || code/100 != 2 {
			f.opts.Logf("ghfake: delivery %s %s.%s → %s: %d %v", d.GUID, d.Event, d.Action, d.URL, code, err)
		}
		f.pending.Done()
	}
}

func send(d *Delivery) (int, error) {
	req, err := http.NewRequest(http.MethodPost, d.URL, bytes.NewReader(d.Body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GitHub-Hookshot/ghfake")
	req.Header.Set("X-GitHub-Event", d.Event)
	req.Header.Set("X-GitHub-Delivery", d.GUID)
	req.Header.Set("X-GitHub-Hook-ID", fmt.Sprint(d.HookID))
	if d.secret != "" {
		m256 := hmac.New(sha256.New, []byte(d.secret))
		m256.Write(d.Body)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m256.Sum(nil)))
		m1 := hmac.New(sha1.New, []byte(d.secret))
		m1.Write(d.Body)
		req.Header.Set("X-Hub-Signature", "sha1="+hex.EncodeToString(m1.Sum(nil)))
	}
	resp, err := hookClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// --- webhook objects ---------------------------------------------------------

// pullWebhook is a pull request as webhooks carry it: GitHub has usually not
// computed mergeability when it sends the event, so mergeable is null and
// mergeable_state "unknown" — a receiver must GET the PR for it.
func (f *Fake) pullWebhook(r *Repo, p *Pull) map[string]any {
	m := f.pullJSON(r, p, true)
	m["merged"] = p.Merged
	m["mergeable"] = nil
	m["rebaseable"] = nil
	m["mergeable_state"] = "unknown"
	m["merged_by"] = f.userJSON(p.MergedBy)
	m["comments"] = len(p.Issue.Comments)
	m["review_comments"] = len(p.ReviewComments)
	m["maintainer_can_modify"] = false
	m["commits"] = 1
	adds, dels := 0, 0
	for _, fl := range p.Files {
		adds += fl.Additions
		dels += fl.Deletions
	}
	m["additions"], m["deletions"], m["changed_files"] = adds, dels, len(p.Files)
	return m
}

func (f *Fake) reviewWebhook(r *Repo, p *Pull, rv *Review) map[string]any {
	m := f.reviewJSON(r, p, rv)
	// Webhooks spell the state lowercase ("changes_requested").
	m["state"] = lower(rv.State)
	return m
}

func (f *Fake) issueWebhook(r *Repo, is *Issue) map[string]any { return f.issueJSON(r, is) }

func (f *Fake) suiteWebhook(r *Repo, s *CheckSuite) map[string]any {
	return map[string]any{
		"id": s.ID, "node_id": nodeID("CS", s.ID), "head_branch": s.HeadBranch, "head_sha": s.HeadSHA,
		"status": s.Status, "conclusion": nullable(s.Conclusion), "url": f.api("/repos/%s/check-suites/%d", r.FullName(), s.ID),
		"before": nil, "after": s.HeadSHA, "pull_requests": f.prRefs(r, s.HeadSHA), "app": f.appJSON(),
		"created_at": ts(s.CreatedAt), "updated_at": ts(s.CreatedAt), "latest_check_runs_count": 1,
		"check_runs_url": f.api("/repos/%s/check-suites/%d/check-runs", r.FullName(), s.ID),
		"head_commit":    f.simpleCommitJSON(r.Commits[s.HeadSHA]), "rerequestable": true, "runs_rerequestable": true,
	}
}

func (f *Fake) emitPull(r *Repo, p *Pull, action string, by *User, extra map[string]any) {
	payload := map[string]any{"number": p.Issue.Number, "pull_request": f.pullWebhook(r, p)}
	for k, v := range extra {
		payload[k] = v
	}
	f.emit(r, "pull_request", action, by, payload)
}

func (f *Fake) emitIssue(r *Repo, is *Issue, action string, by *User, extra map[string]any) {
	payload := map[string]any{"issue": f.issueWebhook(r, is)}
	for k, v := range extra {
		payload[k] = v
	}
	f.emit(r, "issues", action, by, payload)
}

// checkSig verifies an X-Hub-Signature-256 header (for tests and receivers).
func checkSig(secret string, body []byte, header string) bool {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal([]byte(header), []byte("sha256="+hex.EncodeToString(m.Sum(nil))))
}
