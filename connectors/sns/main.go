// Command conductor-sns is the AWS SNS SOURCE connector as an external
// conductor plugin (#59). It runs an HTTP(S) endpoint (and/or a smee.io relay)
// that AWS SNS can deliver a topic subscription to: it auto-confirms the
// subscription, verifies the SNS message signature, and streams a normalized
// "notification" event per delivery to the daemon, which matches it to the
// operator's triggers and resolves the action. It is built ONLY against the
// public SDK + connector-kit (no conductor internals). It is source only —
// publishing to a topic goes through the `aws` connector's verbs.
//
// Config (delivered per start_source, from the connector instance):
//
//	listen: ":9097"                  # HTTP listen address (optional if smee is set)
//	path: "/sns"                     # listener path (default /sns)
//	smee: "https://smee.io/AbC123"   # optional smee.io channel for endpoints with no public URL
//	auto_confirm: true               # GET the SubscribeURL on SubscriptionConfirmation (default true)
//	verify_signature: true           # verify the SNS message signature (default true)
//	allow_unsigned: false            # permit unverified messages when verify_signature is false
//
// At least one of listen/smee must be set. stdout is the RPC transport; all
// logging goes to stderr.
package main

import (
	"bufio"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type sns struct{}

func (sns) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "sns",
		Desc: "AWS SNS HTTP/HTTPS subscription endpoint: auto-confirms the subscription, verifies SNS message signatures, and emits an event per notification (source only — publish via the aws connector). Optional smee.io transport for endpoints without a public URL.",
		Connection: plugin.Schema{
			"listen":           {Type: "string", Desc: "local HTTP listen address for the subscription endpoint (optional if smee is set)"},
			"path":             {Type: "string", Desc: "listener path (default /sns)"},
			"smee":             {Type: "string", Desc: "a smee.io channel URL, e.g. https://smee.io/AbC123 — also (or instead) receive forwarded requests over SSE, so an endpoint with no public URL can still receive SNS deliveries"},
			"auto_confirm":     {Type: "boolean", Desc: "on SubscriptionConfirmation, automatically GET the SubscribeURL (default true)"},
			"verify_signature": {Type: "boolean", Desc: "verify the SNS message signature (default true)"},
			"allow_unsigned":   {Type: "boolean", Desc: "permit unverified messages, only when verify_signature is false (default false)"},
		},
		Events: []plugin.Event{{
			Name: "notification",
			Desc: "an SNS notification was delivered",
			Context: plugin.Schema{
				"topic_arn":    {Type: "string"},
				"subject":      {Type: "string"},
				"message":      {Type: "string"},
				"message_id":   {Type: "string"},
				"timestamp":    {Type: "string"},
				"message_json": {Type: "any", Desc: "Message parsed as JSON, when it is a JSON object"},
			},
			Filters: plugin.Schema{
				"topic_arns": {Type: "list"},
				"subjects":   {Type: "list"},
				"topic_arn":  {Type: "string"},
				"subject":    {Type: "string"},
			},
		}},
		// It fetches SNS signing certs and GETs SubscribeURL under
		// *.amazonaws.com, and optionally relays through smee.io. It never
		// spawns anything.
		Capabilities: plugin.Capabilities{
			Egress: []string{"smee.io:443", "sns.*.amazonaws.com:443", "*.amazonaws.com:443"},
			Spawns: false,
		},
	}
}

func (sns) Invoke(plugin.InvokeRequest) (plugin.InvokeResult, error) {
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "sns is a source connector (no verbs)")
}

func (sns) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	listen := str(cfg["listen"])
	path := strOr(cfg["path"], "/sns")
	smeeURL := str(cfg["smee"])
	if listen == "" && smeeURL == "" {
		return fmt.Errorf("sns: at least one of listen or smee must be configured")
	}

	autoConfirm := boolOr(cfg["auto_confirm"], true)
	verifySig := boolOr(cfg["verify_signature"], true)
	allowUnsigned := boolOr(cfg["allow_unsigned"], false)
	if err := requireSignatureOrAllowUnsigned("sns", verifySig, allowUnsigned); err != nil {
		return err
	}

	certs := newCertCache()
	src := &snsSource{
		autoConfirm:     autoConfirm,
		verifySignature: verifySig,
		emit:            emit,
		dedup:           sourcekit.NewDedup(2048),
		instance:        req.Instance,
		verify:          func(m snsMessage) error { return verifySignature(m, certs) },
	}

	var wg sync.WaitGroup
	var errsMu sync.Mutex
	var errs []error
	recordErr := func(err error) {
		if err == nil {
			return
		}
		errsMu.Lock()
		errs = append(errs, err)
		errsMu.Unlock()
	}

	if listen != "" {
		ln := sourcekit.Listener{Addr: listen, Path: path}
		fmt.Fprintf(os.Stderr, "sns[%s]: listening on %s%s\n", req.Instance, listen, path)
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Secret is intentionally empty: SNS carries its own RSA
			// signature, not an HMAC, so verification happens in handle()
			// against the message body, not the sourcekit listener's HMAC
			// check.
			recordErr(ln.Serve(ctx, func(h http.Header, body []byte) {
				src.handle(h.Get("x-amz-sns-message-type"), body)
			}))
		}()
	}
	if smeeURL != "" {
		fmt.Fprintf(os.Stderr, "sns[%s]: relaying via smee channel %s\n", req.Instance, smeeURL)
		wg.Add(1)
		go func() {
			defer wg.Done()
			src.runSmee(ctx, smeeURL)
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func main() {
	if err := plugin.Serve(sns{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-sns: %v\n", err)
		os.Exit(1)
	}
}

// --- core message handling (shared by the HTTP listener and the smee transport) ---

// snsMessage is the SNS delivery envelope, common to Notification,
// SubscriptionConfirmation, and UnsubscribeConfirmation.
type snsMessage struct {
	Type             string `json:"Type"`
	MessageId        string `json:"MessageId"`
	TopicArn         string `json:"TopicArn"`
	Subject          string `json:"Subject"`
	Message          string `json:"Message"`
	Timestamp        string `json:"Timestamp"`
	SignatureVersion string `json:"SignatureVersion"`
	Signature        string `json:"Signature"`
	SigningCertURL   string `json:"SigningCertURL"`
	Token            string `json:"Token"`
	SubscribeURL     string `json:"SubscribeURL"`
}

// snsSource holds the running state shared by both transports: one handle()
// call per delivery, whichever transport received it.
type snsSource struct {
	autoConfirm     bool
	verifySignature bool
	emit            func(any) error
	dedup           *sourcekit.Dedup
	instance        string
	// verify checks a message's signature; a field (not a bare function call)
	// so tests can stub it out without a network round trip to a real
	// SigningCertURL.
	verify func(snsMessage) error
}

// handle is the ONE core handler for every SNS delivery, called by both the
// HTTP listener (msgType from the x-amz-sns-message-type header) and the smee
// transport (msgType from the forwarded header, or the body's Type field).
func (s *snsSource) handle(msgType string, body []byte) {
	var msg snsMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		fmt.Fprintf(os.Stderr, "sns[%s]: malformed message: %v\n", s.instance, err)
		return
	}
	if msgType == "" {
		msgType = msg.Type
	}
	switch {
	case strings.EqualFold(msgType, "Notification"):
		s.handleNotification(msg)
	case strings.EqualFold(msgType, "SubscriptionConfirmation"):
		s.handleSubscriptionConfirmation(msg)
	case strings.EqualFold(msgType, "UnsubscribeConfirmation"):
		fmt.Fprintf(os.Stderr, "sns[%s]: unsubscribed from topic %s\n", s.instance, msg.TopicArn)
	default:
		fmt.Fprintf(os.Stderr, "sns[%s]: unknown message type %q\n", s.instance, msgType)
	}
}

func (s *snsSource) handleNotification(msg snsMessage) {
	if s.verifySignature {
		if err := s.verify(msg); err != nil {
			fmt.Fprintf(os.Stderr, "sns[%s]: notification signature invalid: %v\n", s.instance, err)
			return
		}
	}
	if msg.MessageId == "" || !s.dedup.Add(msg.MessageId) {
		return
	}
	title := nonEmpty(msg.Subject, "sns notification")
	context := map[string]any{
		"topic_arn":  msg.TopicArn,
		"subject":    msg.Subject,
		"message":    msg.Message,
		"message_id": msg.MessageId,
		"timestamp":  msg.Timestamp,
		// Plural aliases so the documented filter vocabulary (filters:
		// {topic_arns/subjects: [...]}) matches the daemon's generic
		// list-contains filter evaluator.
		"topic_arns": msg.TopicArn,
		"subjects":   msg.Subject,
	}
	if mj := parseMessageJSON(msg.Message); mj != nil {
		context["message_json"] = mj
	}
	_ = s.emit(map[string]any{
		"event":   "notification",
		"kind":    "notification",
		"title":   title,
		"dedup":   msg.MessageId,
		"context": context,
	})
}

// handleSubscriptionConfirmation never emits an event; a subscription
// confirmation is transport plumbing, not something a trigger fires on.
func (s *snsSource) handleSubscriptionConfirmation(msg snsMessage) {
	verified := !s.verifySignature // verification disabled: the startup gate already required allow_unsigned
	if s.verifySignature {
		if err := s.verify(msg); err != nil {
			fmt.Fprintf(os.Stderr, "sns[%s]: subscription confirmation signature invalid: %v\n", s.instance, err)
			verified = false
		} else {
			verified = true
		}
	}
	if !s.autoConfirm {
		return
	}
	// SECURITY GATE: auto-confirm only ever fires once the signature has
	// verified (or verification was explicitly disabled with allow_unsigned,
	// checked at startup). Never GET a SubscribeURL on an unverified message.
	if !verified {
		return
	}
	if msg.SubscribeURL == "" {
		fmt.Fprintf(os.Stderr, "sns[%s]: subscription confirmation missing SubscribeURL\n", s.instance)
		return
	}
	resp, err := http.Get(msg.SubscribeURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sns[%s]: subscribe GET failed: %v\n", s.instance, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		fmt.Fprintf(os.Stderr, "sns[%s]: confirmed subscription for topic %s\n", s.instance, msg.TopicArn)
	} else {
		fmt.Fprintf(os.Stderr, "sns[%s]: subscribe GET returned %s\n", s.instance, resp.Status)
	}
}

// parseMessageJSON returns Message parsed as a JSON object, or nil when it
// isn't JSON (or isn't an object) — SNS notifications commonly carry a plain
// string, so this is best-effort enrichment, not a requirement.
func parseMessageJSON(message string) any {
	message = strings.TrimSpace(message)
	if message == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(message), &v); err != nil {
		return nil
	}
	if _, ok := v.(map[string]any); !ok {
		return nil
	}
	return v
}

// --- SNS signature verification ---

// canonicalString builds the exact string SNS signs, in the documented key
// order, including only the keys present in the message. Getting this wrong
// silently breaks verification for every message of one type, so the key
// order is a literal transcription of AWS's spec, not a derived list.
func canonicalString(msg snsMessage) string {
	var b strings.Builder
	add := func(key, val string) {
		b.WriteString(key)
		b.WriteByte('\n')
		b.WriteString(val)
		b.WriteByte('\n')
	}
	if strings.EqualFold(msg.Type, "Notification") {
		add("Message", msg.Message)
		add("MessageId", msg.MessageId)
		if msg.Subject != "" {
			add("Subject", msg.Subject)
		}
		add("Timestamp", msg.Timestamp)
		add("TopicArn", msg.TopicArn)
		add("Type", msg.Type)
		return b.String()
	}
	// SubscriptionConfirmation / UnsubscribeConfirmation.
	add("Message", msg.Message)
	add("MessageId", msg.MessageId)
	add("SubscribeURL", msg.SubscribeURL)
	add("Timestamp", msg.Timestamp)
	add("Token", msg.Token)
	add("TopicArn", msg.TopicArn)
	add("Type", msg.Type)
	return b.String()
}

// verifySignature validates msg's signature against the cert at
// SigningCertURL, enforcing the host allowlist before ever fetching it.
func verifySignature(msg snsMessage, certs *certCache) error {
	if err := validateCertURLHost(msg.SigningCertURL); err != nil {
		return err
	}
	pub, err := certs.get(msg.SigningCertURL)
	if err != nil {
		return err
	}
	return verifySignatureWithKey(msg, pub)
}

// verifySignatureWithKey is the pure crypto step — canonical string, hash by
// SignatureVersion, rsa.VerifyPKCS1v15 — split out from cert fetching so it can
// be exercised directly against an in-test key.
func verifySignatureWithKey(msg snsMessage, pub *rsa.PublicKey) error {
	sig, err := base64.StdEncoding.DecodeString(msg.Signature)
	if err != nil {
		return fmt.Errorf("sns: bad signature encoding: %w", err)
	}
	str := canonicalString(msg)
	switch msg.SignatureVersion {
	case "2":
		sum := sha256.Sum256([]byte(str))
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
			return fmt.Errorf("sns: signature verify (v2): %w", err)
		}
	case "", "1":
		sum := sha1.Sum([]byte(str))
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA1, sum[:], sig); err != nil {
			return fmt.Errorf("sns: signature verify (v1): %w", err)
		}
	default:
		return fmt.Errorf("sns: unsupported SignatureVersion %q", msg.SignatureVersion)
	}
	return nil
}

// validateCertURLHost requires SigningCertURL to be https and hosted on
// amazonaws.com. Without this, a forged message pointing SigningCertURL at an
// attacker-controlled cert would "verify" against a signature the attacker
// made themselves — the allowlist is what makes fetching the cert safe.
func validateCertURLHost(rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("sns: message has no SigningCertURL")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("sns: invalid SigningCertURL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("sns: SigningCertURL must be https, got %q", rawURL)
	}
	host := strings.ToLower(u.Hostname())
	if host != "amazonaws.com" && !strings.HasSuffix(host, ".amazonaws.com") {
		return fmt.Errorf("sns: SigningCertURL host %q is not an amazonaws.com host", u.Hostname())
	}
	return nil
}

// certCache fetches and caches SNS signing certs by URL, so a busy topic
// doesn't refetch the same cert for every notification.
type certCache struct {
	mu sync.Mutex
	m  map[string]*rsa.PublicKey
}

func newCertCache() *certCache { return &certCache{m: map[string]*rsa.PublicKey{}} }

func (c *certCache) get(certURL string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	if pub, ok := c.m[certURL]; ok {
		c.mu.Unlock()
		return pub, nil
	}
	c.mu.Unlock()
	pub, err := fetchCert(certURL)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.m[certURL] = pub
	c.mu.Unlock()
	return pub, nil
}

// fetchCert has no host opinion of its own — validateCertURLHost is the gate,
// called before this by every real code path — so it can be exercised
// directly in tests against an httptest.Server.
func fetchCert(certURL string) (*rsa.PublicKey, error) {
	resp, err := http.Get(certURL)
	if err != nil {
		return nil, fmt.Errorf("sns: fetch signing cert: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sns: fetch signing cert: status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("sns: read signing cert: %w", err)
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, fmt.Errorf("sns: signing cert: no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("sns: signing cert: %w", err)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("sns: signing cert is not RSA")
	}
	return pub, nil
}

// --- smee.io transport (stdlib SSE client) ---

// runSmee connects to a smee.io channel and feeds every forwarded request into
// handle(), reconnecting with backoff until ctx is cancelled.
func (s *snsSource) runSmee(ctx context.Context, smeeURL string) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for ctx.Err() == nil {
		if err := s.smeeOnce(ctx, smeeURL); err != nil {
			fmt.Fprintf(os.Stderr, "sns[%s]: smee stream error: %v\n", s.instance, err)
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// smeeOnce holds one smee connection open, parsing the SSE stream into
// forwarded-request payloads until the connection drops or ctx cancels.
func (s *snsSource) smeeOnce(ctx context.Context, smeeURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, smeeURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("smee: unexpected status %s", resp.Status)
	}
	fmt.Fprintf(os.Stderr, "sns[%s]: smee connected\n", s.instance)
	defer fmt.Fprintf(os.Stderr, "sns[%s]: smee disconnected\n", s.instance)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var data strings.Builder
	flush := func() {
		if data.Len() == 0 {
			return
		}
		payload := data.String()
		data.Reset()
		if msgType, body, ok := parseSmeePayload([]byte(payload)); ok {
			s.handle(msgType, body)
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			flush()
		default:
			// event:, id:, retry:, and comments (":...") carry no SNS
			// payload — the "ready"/keep-alive events smee sends land here
			// and are silently ignored.
		}
	}
	flush()
	return scanner.Err()
}

// parseSmeePayload extracts the SNS message type and body bytes from one
// smee-forwarded `data:` JSON payload. smee delivers the original request as
// headers at the top level, plus body/query/host/timestamp; body may arrive as
// a nested JSON object or as a raw string. Returns ok=false for a payload with
// no body (smee's own ready/keep-alive events).
func parseSmeePayload(raw []byte) (msgType string, body []byte, ok bool) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", nil, false
	}
	bodyVal, present := payload["body"]
	if !present {
		return "", nil, false
	}
	switch b := bodyVal.(type) {
	case string:
		body = []byte(b)
	case nil:
		return "", nil, false
	default:
		marshaled, err := json.Marshal(b)
		if err != nil {
			return "", nil, false
		}
		body = marshaled
	}
	msgType = headerValue(payload, "x-amz-sns-message-type")
	if msgType == "" {
		var m snsMessage
		_ = json.Unmarshal(body, &m)
		msgType = m.Type
	}
	return msgType, body, true
}

// headerValue looks up a smee-forwarded header case-insensitively — smee
// preserves the original request's header casing, which does not necessarily
// match what SNS actually sent.
func headerValue(payload map[string]any, key string) string {
	for k, v := range payload {
		if strings.EqualFold(k, key) {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

// --- option helpers ---

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}

func boolOr(v any, def bool) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		switch strings.ToLower(x) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	return def
}

func nonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// requireSignatureOrAllowUnsigned refuses to start an SNS source that would
// trust deliveries — and auto-confirm subscriptions — without ever checking
// the SNS message signature.
//
// verify_signature defaults to true, so this only bites when an operator
// turns it off. Turning it off with no other guard means handle() trusts
// whatever body arrives at the listen address (or gets relayed through smee)
// as a real AWS SNS delivery, and — if auto_confirm is also on — GETs
// whatever SubscribeURL a forged SubscriptionConfirmation names. That is
// remote trigger injection AND an open subscription-confirmation oracle, with
// no signal that either happened. A disabled signature check is far more
// often a mistake (or a "get it working first" step never revisited) than a
// deliberate choice, so it fails closed; `allow_unsigned: true` is the
// explicit, greppable way to say you meant it — because something else in
// front of this endpoint already authenticates it.
func requireSignatureOrAllowUnsigned(who string, verifySig, allowUnsigned bool) error {
	if verifySig {
		return nil
	}
	if allowUnsigned {
		fmt.Fprintf(os.Stderr, "%s: allow_unsigned is set and verify_signature is false — trusting UNVERIFIED SNS deliveries and auto-confirming subscriptions blind\n", who)
		return nil
	}
	return fmt.Errorf("%s: verify_signature is false with no allow_unsigned — an endpoint that trusts unverified deliveries (and auto-confirms subscriptions) is a vector for remote trigger injection and subscription abuse. Leave verify_signature: true (the default), or set allow_unsigned: true if you genuinely front this with something else that authenticates", who)
}
