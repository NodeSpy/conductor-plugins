// Command conductor-email is the email connector as a standalone external
// conductor plugin (#59). It sends mail over SMTP (verbs) and polls an IMAP
// mailbox for new messages (source), so an operator's own mail account — not
// a SaaS webhook — is the transport in both directions.
//
// Built ONLY against the public SDK (pkg/plugin) plus the standard library:
// net/smtp drives outbound mail, and net + crypto/tls back a minimal,
// hand-rolled IMAP4rev1 client for the inbound poll loop (the stdlib has no
// IMAP package). No third-party or conductor-internal import.
//
// Connection (used for both Invoke and StartSource):
//
//	smtp:
//	  host: smtp.example.com
//	  port: 587                 # default 587
//	  username: bot@example.com
//	  password: <secret>
//	  from: bot@example.com     # default From/envelope-sender when a verb omits one
//	  tls: starttls             # starttls (default) | tls (implicit, port 465) | none
//	imap:
//	  host: imap.example.com
//	  port: 993                 # default 993
//	  username: bot@example.com
//	  password: <secret>
//	  mailbox: INBOX            # default INBOX
//	  tls: true                 # default true (implicit TLS); false = plaintext + STARTTLS
//	  poll_interval: 60s        # default 60s
//	  mark_seen: true           # default true — \Seen after emitting
//	  search: UNSEEN            # default UNSEEN — the IMAP SEARCH criteria polled each round
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/smtp"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

type emailPlugin struct{}

func (emailPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "email",
		Desc: "Email: send mail over SMTP (verbs); poll an IMAP mailbox for new messages (source). " +
			"Hand-rolled minimal IMAP4rev1 client — no third-party mail library. " +
			"Egress is operator-specific (your own smtp/imap hosts); narrow it with the instance's network:.",
		Connection: plugin.Schema{
			"smtp": {Type: "map", Desc: "host, port (default 587), username, password, from, tls (starttls|tls|none, default starttls)"},
			"imap": {Type: "map", Desc: "host, port (default 993), username, password, mailbox (default INBOX), tls (default true), poll_interval (default 60s), mark_seen (default true), search (default UNSEEN)"},
		},
		Events: []plugin.Event{
			{
				Name: "message", Desc: "a new message matched the IMAP search criteria",
				Filters: plugin.Schema{
					"froms":    {Type: "list", Desc: "match if From is one of these addresses"},
					"from":     {Type: "string", Desc: "match if From equals this address"},
					"subjects": {Type: "list", Desc: "match if Subject contains one of these"},
					"subject":  {Type: "string", Desc: "match if Subject contains this"},
				},
				Context: plugin.Schema{
					"from":       {Type: "string"},
					"to":         {Type: "string"},
					"subject":    {Type: "string"},
					"date":       {Type: "string"},
					"message_id": {Type: "string"},
					"body":       {Type: "string"},
					"uid":        {Type: "integer"},
					"mailbox":    {Type: "string"},
				},
			},
		},
		Verbs: []plugin.Verb{
			{
				Name: "send", Desc: "send an email over SMTP",
				Options: plugin.Schema{
					"to":       {Type: "any", Required: true, Desc: "recipient address, or list of addresses"},
					"cc":       {Type: "list", Desc: "Cc addresses"},
					"bcc":      {Type: "list", Desc: "Bcc addresses (not written into the message headers)"},
					"subject":  {Type: "string"},
					"body":     {Type: "string", Desc: "plain-text body; multipart/alternative with html if both are set"},
					"html":     {Type: "string", Desc: "HTML body"},
					"from":     {Type: "string", Desc: "override connection.smtp.from"},
					"reply_to": {Type: "string"},
					"headers":  {Type: "map", Desc: "additional raw header fields"},
				},
				Outputs: plugin.Schema{"sent": {Type: "boolean"}, "message_id": {Type: "string"}},
			},
			{
				Name: "send_raw", Desc: "send a pre-built RFC 5322 message verbatim",
				Options: plugin.Schema{
					"raw":  {Type: "string", Required: true, Desc: "the full RFC 5322 message, headers and body"},
					"to":   {Type: "any", Required: true, Desc: "envelope recipient address, or list of addresses"},
					"from": {Type: "string", Desc: "override connection.smtp.from (envelope sender)"},
				},
				Outputs: plugin.Schema{"sent": {Type: "boolean"}},
			},
		},
		// Egress is operator-specific: the smtp/imap host:port an operator
		// points this instance at, not a fixed vendor API. Left empty; the
		// operator's network: narrows what the plugin may actually dial, and
		// docs/connectors/email.md documents the pattern.
		Capabilities: plugin.Capabilities{Egress: []string{}},
	}
}

func (emailPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	smtpCfg, err := parseSMTPConn(asMap(req.Connection["smtp"]))
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "connection.smtp: "+err.Error())
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	switch req.Verb {
	case "send":
		return invokeSend(smtpCfg, o)
	case "send_raw":
		return invokeSendRaw(smtpCfg, o)
	default:
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeMethodNotFound, "unknown verb "+req.Verb)
	}
}

func invokeSend(cfg smtpConfig, o map[string]any) (plugin.InvokeResult, error) {
	opts := msgOpts{
		From:    strOr(str(o["from"]), cfg.From),
		To:      strList(o["to"]),
		Cc:      strList(o["cc"]),
		Bcc:     strList(o["bcc"]),
		Subject: str(o["subject"]),
		Body:    str(o["body"]),
		HTML:    str(o["html"]),
		ReplyTo: str(o["reply_to"]),
		Headers: strMap(o["headers"]),
		Date:    time.Now(),
	}
	if opts.From == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "from is required (set options.from or connection.smtp.from)")
	}
	if len(opts.To) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "to is required")
	}
	raw, msgID, err := buildMessage(opts)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, err.Error())
	}
	rcpts := append(append(append([]string{}, opts.To...), opts.Cc...), opts.Bcc...)
	if err := sendSMTP(cfg, opts.From, rcpts, raw); err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "send: "+err.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"sent": true, "message_id": msgID}}, nil
}

func invokeSendRaw(cfg smtpConfig, o map[string]any) (plugin.InvokeResult, error) {
	raw := str(o["raw"])
	if raw == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "raw is required")
	}
	to := strList(o["to"])
	if len(to) == 0 {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "to is required")
	}
	from := strOr(str(o["from"]), cfg.From)
	if from == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "from is required (set options.from or connection.smtp.from)")
	}
	if err := sendSMTP(cfg, from, to, []byte(raw)); err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "send_raw: "+err.Error())
	}
	return plugin.InvokeResult{Outputs: map[string]any{"sent": true}}, nil
}

// --- SMTP: message building (pure) ---

// msgOpts is buildMessage's input: every field needed to render an RFC 5322
// message, with nothing reached from the environment — Date and MessageID
// are overridable so tests can pin an exact byte-for-byte expectation.
type msgOpts struct {
	From      string
	To        []string
	Cc        []string
	Bcc       []string // envelope-only: never written into headers
	ReplyTo   string
	Subject   string
	Body      string
	HTML      string
	Headers   map[string]string
	Date      time.Time
	MessageID string // override; generated when empty
	Boundary  string // override; generated when empty and both Body+HTML set
}

// buildMessage renders an RFC 5322 message: multipart/alternative when both
// Body and HTML are set, otherwise a single text/plain or text/html part. It
// touches no network and no clock beyond what Date/MessageID/Boundary already
// carry, so it is exercised directly by tests with no server involved.
func buildMessage(o msgOpts) (raw []byte, messageID string, err error) {
	if o.Body == "" && o.HTML == "" {
		return nil, "", fmt.Errorf("body or html is required")
	}
	msgID := o.MessageID
	if msgID == "" {
		msgID = generateMessageID(o.From)
	}
	date := o.Date
	if date.IsZero() {
		date = time.Now()
	}
	var buf bytes.Buffer
	buf.WriteString("Date: " + date.Format(time.RFC1123Z) + "\r\n")
	buf.WriteString("From: " + o.From + "\r\n")
	if len(o.To) > 0 {
		buf.WriteString("To: " + strings.Join(o.To, ", ") + "\r\n")
	}
	if len(o.Cc) > 0 {
		buf.WriteString("Cc: " + strings.Join(o.Cc, ", ") + "\r\n")
	}
	if o.ReplyTo != "" {
		buf.WriteString("Reply-To: " + o.ReplyTo + "\r\n")
	}
	buf.WriteString("Subject: " + o.Subject + "\r\n")
	buf.WriteString("Message-ID: " + msgID + "\r\n")
	buf.WriteString("MIME-Version: 1.0\r\n")
	for _, k := range sortedKeys(o.Headers) {
		buf.WriteString(k + ": " + o.Headers[k] + "\r\n")
	}
	switch {
	case o.Body != "" && o.HTML != "":
		boundary := o.Boundary
		if boundary == "" {
			boundary = generateBoundary()
		}
		buf.WriteString("Content-Type: multipart/alternative; boundary=" + boundary + "\r\n\r\n")
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(o.Body + "\r\n\r\n")
		buf.WriteString("--" + boundary + "\r\n")
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(o.HTML + "\r\n\r\n")
		buf.WriteString("--" + boundary + "--\r\n")
	case o.HTML != "":
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(o.HTML)
	default:
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(o.Body)
	}
	return buf.Bytes(), msgID, nil
}

func generateMessageID(from string) string {
	host := "localhost"
	if _, d, ok := strings.Cut(from, "@"); ok && d != "" {
		host = d
	}
	return fmt.Sprintf("<%d.%d@%s>", time.Now().UnixNano(), rand.Int63(), host)
}

func generateBoundary() string {
	return fmt.Sprintf("conductor-email-%d-%d", time.Now().UnixNano(), rand.Int63())
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- SMTP: sending ---

type smtpConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	TLS      string // starttls (default) | tls | none
}

func parseSMTPConn(m map[string]any) (smtpConfig, error) {
	c := smtpConfig{
		Host:     str(m["host"]),
		Port:     intOr(m["port"], 587),
		Username: str(m["username"]),
		Password: str(m["password"]),
		From:     str(m["from"]),
		TLS:      strOr(str(m["tls"]), "starttls"),
	}
	if c.Host == "" {
		return c, fmt.Errorf("host is required")
	}
	switch c.TLS {
	case "starttls", "tls", "none":
	default:
		return c, fmt.Errorf("tls must be starttls, tls, or none, got %q", c.TLS)
	}
	return c, nil
}

// sendSMTP delivers raw to rcpts, choosing the transport by cfg.TLS:
// "starttls" (default, port 587) uses smtp.SendMail, which opportunistically
// STARTTLS-upgrades; "tls" (port 465) dials straight into implicit TLS; "none"
// is a plaintext connection for local/test relays only.
func sendSMTP(cfg smtpConfig, from string, rcpts []string, raw []byte) error {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}
	switch cfg.TLS {
	case "tls":
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: cfg.Host})
		if err != nil {
			return err
		}
		defer conn.Close()
		client, err := smtp.NewClient(conn, cfg.Host)
		if err != nil {
			return err
		}
		defer client.Close()
		return smtpSendSequence(client, auth, from, rcpts, raw)
	case "none":
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return err
		}
		defer conn.Close()
		client, err := smtp.NewClient(conn, cfg.Host)
		if err != nil {
			return err
		}
		defer client.Close()
		return smtpSendSequence(client, auth, from, rcpts, raw)
	default: // "starttls"
		return smtp.SendMail(addr, auth, from, rcpts, raw)
	}
}

func smtpSendSequence(client *smtp.Client, auth smtp.Auth, from string, rcpts []string, raw []byte) error {
	if auth != nil {
		if ok, _ := client.Extension("AUTH"); ok {
			if err := client.Auth(auth); err != nil {
				return err
			}
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, r := range rcpts {
		if err := client.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// --- IMAP: source (poll loop) ---

type imapConfig struct {
	Host         string
	Port         int
	Username     string
	Password     string
	Mailbox      string
	TLS          bool
	PollInterval time.Duration
	MarkSeen     bool
	Search       string
}

func parseIMAPConn(m map[string]any) (imapConfig, error) {
	c := imapConfig{
		Host:         str(m["host"]),
		Port:         intOr(m["port"], 993),
		Username:     str(m["username"]),
		Password:     str(m["password"]),
		Mailbox:      strOr(str(m["mailbox"]), "INBOX"),
		TLS:          boolOr(m["tls"], true),
		PollInterval: 60 * time.Second,
		MarkSeen:     boolOr(m["mark_seen"], true),
		Search:       strOr(str(m["search"]), "UNSEEN"),
	}
	if c.Host == "" {
		return c, fmt.Errorf("host is required")
	}
	if d, err := toDuration(m["poll_interval"]); err != nil {
		return c, fmt.Errorf("poll_interval: %w", err)
	} else if d > 0 {
		c.PollInterval = d
	}
	return c, nil
}

func (emailPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg, err := parseIMAPConn(asMap(req.Config["imap"]))
	if err != nil {
		return fmt.Errorf("email: connection.imap: %w", err)
	}
	dedup := sourcekit.NewDedup(4096)
	fmt.Fprintf(os.Stderr, "email[%s]: polling %s (mailbox %s) every %s\n", req.Instance, cfg.Host, cfg.Mailbox, cfg.PollInterval)
	for {
		if ctx.Err() != nil {
			return nil
		}
		client, err := dialIMAP(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "email[%s]: imap connect: %v\n", req.Instance, err)
			if !sleepCtx(ctx, 15*time.Second) {
				return nil
			}
			continue
		}
		err = pollIMAP(ctx, client, cfg, dedup, emit)
		client.conn.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "email[%s]: imap: %v (reconnecting)\n", req.Instance, err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if !sleepCtx(ctx, 15*time.Second) {
			return nil
		}
	}
}

// pollIMAP runs the poll loop on one live connection: SEARCH, FETCH+emit each
// new UID, optionally mark \Seen, sleep, repeat. It returns (nil, ctx done) on
// clean shutdown or a non-nil error on any IMAP/network failure, which the
// caller treats as "reconnect".
func pollIMAP(ctx context.Context, c *imapClient, cfg imapConfig, dedup *sourcekit.Dedup, emit func(any) error) error {
	for {
		uids, err := c.search(cfg.Search)
		if err != nil {
			return err
		}
		for _, uid := range uids {
			headers, body, err := c.fetch(uid)
			if err != nil {
				return err
			}
			key := headers["message-id"]
			if key == "" {
				key = fmt.Sprintf("uid:%d@%s", uid, cfg.Mailbox)
			}
			if !dedup.Add(key) {
				continue
			}
			_ = emit(messageEvent(cfg.Mailbox, uid, headers, body))
			if cfg.MarkSeen {
				if err := c.markSeen(uid); err != nil {
					return err
				}
			}
		}
		if !sleepCtx(ctx, cfg.PollInterval) {
			return nil
		}
	}
}

// messageEvent is the normalized "message" event streamed to the daemon.
// froms/subjects are plural aliases of from/subject so the documented filter
// vocabulary (filters: { froms: [...] }) matches the daemon's generic
// list-contains filter evaluator, the same convention the sentry plugin uses
// for levels/projects/environments.
func messageEvent(mailbox string, uid uint32, headers map[string]string, body string) map[string]any {
	from, to, subject := headers["from"], headers["to"], headers["subject"]
	return map[string]any{
		"event": "message",
		"kind":  "message",
		"title": fmt.Sprintf("email from %s: %s", from, subject),
		"dedup": nonEmpty(headers["message-id"], fmt.Sprintf("uid:%d@%s", uid, mailbox)),
		"context": map[string]any{
			"from": from, "to": to, "subject": subject, "date": headers["date"],
			"message_id": headers["message-id"], "body": body, "uid": uid, "mailbox": mailbox,
			"froms": from, "subjects": subject,
		},
	}
}

// sleepCtx sleeps d or returns early (false) if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// --- IMAP: minimal hand-rolled IMAP4rev1 client ---

// imapClient is a single logged-in, mailbox-selected IMAP session.
type imapClient struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	tagN int
}

func newIMAPClient(conn net.Conn) *imapClient {
	return &imapClient{conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn)}
}

func (c *imapClient) nextTag() string {
	c.tagN++
	return fmt.Sprintf("a%04d", c.tagN)
}

// cmd sends one tagged command and returns the tag used, for readUntilTagged.
func (c *imapClient) cmd(format string, args ...any) (string, error) {
	tag := c.nextTag()
	line := fmt.Sprintf(format, args...)
	if _, err := c.w.WriteString(tag + " " + line + "\r\n"); err != nil {
		return tag, err
	}
	return tag, c.w.Flush()
}

// readGreeting consumes the server's initial "* OK ..." banner.
func (c *imapClient) readGreeting() error {
	line, _, err := readIMAPLine(c.r)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "*") {
		return fmt.Errorf("unexpected greeting: %q", line)
	}
	return nil
}

// readUntilTagged reads response lines until the one tagged with tag,
// returning every untagged line (with its literal spans) seen along the way
// plus the tagged line's status word (OK/NO/BAD).
func (c *imapClient) readUntilTagged(tag string) (untagged []string, literals [][]literalSpan, status string, err error) {
	for {
		line, lits, err := readIMAPLine(c.r)
		if err != nil {
			return untagged, literals, "", err
		}
		if rest, ok := strings.CutPrefix(line, tag+" "); ok {
			status, _, _ = strings.Cut(rest, " ")
			return untagged, literals, status, nil
		}
		untagged = append(untagged, line)
		literals = append(literals, lits)
	}
}

func dialIMAP(cfg imapConfig) (*imapClient, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	var conn net.Conn
	var err error
	if cfg.TLS {
		conn, err = tls.Dial("tcp", addr, &tls.Config{ServerName: cfg.Host})
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	c := newIMAPClient(conn)
	if err := c.readGreeting(); err != nil {
		conn.Close()
		return nil, err
	}
	if !cfg.TLS {
		tag, err := c.cmd("STARTTLS")
		if err != nil {
			conn.Close()
			return nil, err
		}
		_, _, status, err := c.readUntilTagged(tag)
		if err != nil || status != "OK" {
			conn.Close()
			return nil, fmt.Errorf("starttls failed: %v", err)
		}
		tlsConn := tls.Client(conn, &tls.Config{ServerName: cfg.Host})
		if err := tlsConn.Handshake(); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tlsConn
		c = newIMAPClient(conn)
	}
	tag, err := c.cmd("LOGIN %s %s", imapQuote(cfg.Username), imapQuote(cfg.Password))
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, _, status, err := c.readUntilTagged(tag); err != nil || status != "OK" {
		conn.Close()
		return nil, fmt.Errorf("login failed: %v", err)
	}
	tag, err = c.cmd("SELECT %s", imapQuote(cfg.Mailbox))
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, _, status, err := c.readUntilTagged(tag); err != nil || status != "OK" {
		conn.Close()
		return nil, fmt.Errorf("select %s failed: %v", cfg.Mailbox, err)
	}
	return c, nil
}

func (c *imapClient) search(criteria string) ([]uint32, error) {
	tag, err := c.cmd("UID SEARCH %s", criteria)
	if err != nil {
		return nil, err
	}
	untagged, _, status, err := c.readUntilTagged(tag)
	if err != nil {
		return nil, err
	}
	if status != "OK" {
		return nil, fmt.Errorf("search failed: %s", status)
	}
	var uids []uint32
	for _, line := range untagged {
		uids = append(uids, parseSearchLine(line)...)
	}
	return uids, nil
}

func (c *imapClient) fetch(uid uint32) (headers map[string]string, body string, err error) {
	tag, err := c.cmd("UID FETCH %d (BODY.PEEK[HEADER.FIELDS (FROM TO SUBJECT DATE MESSAGE-ID)] BODY.PEEK[TEXT])", uid)
	if err != nil {
		return nil, "", err
	}
	untagged, literals, status, err := c.readUntilTagged(tag)
	if err != nil {
		return nil, "", err
	}
	if status != "OK" {
		return nil, "", fmt.Errorf("fetch %d failed: %s", uid, status)
	}
	for i, line := range untagged {
		if strings.Contains(line, "FETCH") {
			return parseFetchLine(line, literals[i])
		}
	}
	return nil, "", fmt.Errorf("fetch %d: no FETCH data returned", uid)
}

func (c *imapClient) markSeen(uid uint32) error {
	tag, err := c.cmd(`UID STORE %d +FLAGS (\Seen)`, uid)
	if err != nil {
		return err
	}
	if _, _, status, err := c.readUntilTagged(tag); err != nil || status != "OK" {
		return fmt.Errorf("store %d failed: %v", uid, err)
	}
	return nil
}

// imapQuote renders a string as an IMAP quoted string literal.
func imapQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// --- IMAP wire parsing (pure; unit-tested against canned server strings) ---

// literalSpan is a byte range within a logical line's accumulated text that
// came from an IMAP literal ({N}\r\n<N bytes>) rather than plain line text.
type literalSpan struct {
	Start, Len int
}

// literalSizeRe matches a trailing IMAP literal marker "{123}" on a line.
var literalSizeRe = regexp.MustCompile(`\{(\d+)\}$`)

func literalSize(line string) (int, bool) {
	m := literalSizeRe.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// readIMAPLine reads one logical IMAP response line: CRLF-terminated text
// that may embed one or more {N} literals, each an exact byte count that can
// itself contain CRLFs. It keeps consuming line segments as long as the
// segment just read ends in a literal marker, so the returned text is the
// full logical line with literal bytes spliced in verbatim, plus the byte
// ranges (within that text) each literal occupies — callers use the ranges to
// pull literal content out unambiguously, without re-scanning for markers
// that could coincidentally appear inside message content.
func readIMAPLine(r *bufio.Reader) (text string, literals []literalSpan, err error) {
	var sb strings.Builder
	for {
		seg, err := r.ReadString('\n')
		if err != nil {
			return "", nil, err
		}
		sb.WriteString(seg)
		trimmed := strings.TrimRight(seg, "\r\n")
		n, ok := literalSize(trimmed)
		if !ok {
			break
		}
		start := sb.Len()
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", nil, err
		}
		sb.Write(buf)
		literals = append(literals, literalSpan{Start: start, Len: n})
	}
	full := sb.String()
	full = strings.TrimRight(full, "\r\n")
	return full, literals, nil
}

// parseSearchLine parses one untagged SEARCH response line ("* SEARCH 1 2 3")
// into UIDs (blank/malformed lines yield nil).
func parseSearchLine(line string) []uint32 {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "*" || fields[1] != "SEARCH" {
		return nil
	}
	var uids []uint32
	for _, f := range fields[2:] {
		n, err := strconv.ParseUint(f, 10, 32)
		if err != nil {
			continue
		}
		uids = append(uids, uint32(n))
	}
	return uids
}

// parseFetchLine parses one untagged FETCH response line into the header
// block and body text: literals[0] is the HEADER.FIELDS literal, literals[1]
// is the BODY[TEXT] literal — the fixed order this client always requests
// them in (see (*imapClient).fetch), so no marker-text scanning is needed to
// tell them apart.
func parseFetchLine(text string, literals []literalSpan) (headers map[string]string, body string, err error) {
	if len(literals) < 2 {
		return nil, "", fmt.Errorf("fetch response missing expected literals (got %d)", len(literals))
	}
	h := literals[0]
	b := literals[1]
	if h.Start+h.Len > len(text) || b.Start+b.Len > len(text) {
		return nil, "", fmt.Errorf("fetch response literal out of range")
	}
	headerBlock := text[h.Start : h.Start+h.Len]
	body = text[b.Start : b.Start+b.Len]
	return parseHeaderBlock(headerBlock), strings.TrimRight(body, "\r\n"), nil
}

// parseHeaderBlock parses an RFC 5322 header block (as returned by
// BODY[HEADER.FIELDS (...)]) into a lower-cased-key map, unfolding
// continuation lines (leading whitespace).
func parseHeaderBlock(block string) map[string]string {
	headers := map[string]string{}
	lines := strings.Split(strings.ReplaceAll(block, "\r\n", "\n"), "\n")
	var curKey string
	for _, line := range lines {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && curKey != "" {
			headers[curKey] = strings.TrimSpace(headers[curKey] + " " + strings.TrimSpace(line))
			continue
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		curKey = strings.ToLower(strings.TrimSpace(name))
		headers[curKey] = strings.TrimSpace(val)
	}
	return headers
}

// --- small option helpers (stdlib only) ---

func str(v any) string { s, _ := v.(string); return s }

func strOr(v string, def string) string {
	if v != "" {
		return v
	}
	return def
}

func boolOr(v any, def bool) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		switch x {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	return def
}

func intOr(v any, def int) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		if n, err := strconv.Atoi(x); err == nil {
			return n
		}
	}
	return def
}

func toDuration(v any) (time.Duration, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case string:
		if x == "" {
			return 0, nil
		}
		return time.ParseDuration(x)
	case float64:
		return time.Duration(x * float64(time.Second)), nil
	case int:
		return time.Duration(x) * time.Second, nil
	case int64:
		return time.Duration(x) * time.Second, nil
	}
	return 0, fmt.Errorf("invalid duration %v", v)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// strList accepts a string, []string, or []any and returns a non-empty slice.
func strList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func strMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		} else {
			out[k] = fmt.Sprintf("%v", val)
		}
	}
	return out
}

func nonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func main() {
	if err := plugin.Serve(emailPlugin{}); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-email:", err)
		os.Exit(1)
	}
}
