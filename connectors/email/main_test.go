package main

import (
	"bufio"
	"strconv"
	"strings"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// --- buildMessage: headers + multipart ---

func TestBuildMessagePlainText(t *testing.T) {
	raw, msgID, err := buildMessage(msgOpts{
		From: "bot@example.com", To: []string{"a@example.com", "b@example.com"},
		Subject: "hi", Body: "hello there",
		Date: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), MessageID: "<fixed@example.com>",
	})
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	if msgID != "<fixed@example.com>" {
		t.Fatalf("message id: got %q", msgID)
	}
	s := string(raw)
	for _, want := range []string{
		"From: bot@example.com\r\n",
		"To: a@example.com, b@example.com\r\n",
		"Subject: hi\r\n",
		"Message-ID: <fixed@example.com>\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if !strings.HasSuffix(s, "hello there") {
		t.Errorf("body not at end: %s", s)
	}
	if strings.Contains(s, "multipart") {
		t.Errorf("should not be multipart: %s", s)
	}
}

func TestBuildMessageHTMLOnly(t *testing.T) {
	raw, _, err := buildMessage(msgOpts{
		From: "bot@example.com", To: []string{"a@example.com"},
		Subject: "hi", HTML: "<b>hi</b>", MessageID: "<x@y>",
	})
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, "Content-Type: text/html; charset=utf-8\r\n") {
		t.Errorf("expected html content-type: %s", s)
	}
	if !strings.HasSuffix(s, "<b>hi</b>") {
		t.Errorf("html body not at end: %s", s)
	}
}

func TestBuildMessageMultipartAlternative(t *testing.T) {
	raw, _, err := buildMessage(msgOpts{
		From: "bot@example.com", To: []string{"a@example.com"},
		Subject: "hi", Body: "plain", HTML: "<b>html</b>",
		MessageID: "<x@y>", Boundary: "BOUND1",
	})
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, `Content-Type: multipart/alternative; boundary=BOUND1`) {
		t.Errorf("missing multipart header: %s", s)
	}
	// Both parts present, each preceded by its own boundary marker, and the
	// message closes with the terminating boundary.
	if !strings.Contains(s, "--BOUND1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nplain") {
		t.Errorf("missing plain part: %s", s)
	}
	if !strings.Contains(s, "--BOUND1\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<b>html</b>") {
		t.Errorf("missing html part: %s", s)
	}
	if !strings.HasSuffix(s, "--BOUND1--\r\n") {
		t.Errorf("missing closing boundary: %s", s)
	}
}

func TestBuildMessageRequiresBodyOrHTML(t *testing.T) {
	if _, _, err := buildMessage(msgOpts{From: "a@b.com", To: []string{"c@d.com"}}); err == nil {
		t.Fatal("expected error when neither body nor html is set")
	}
}

func TestBuildMessageCustomHeadersSorted(t *testing.T) {
	raw, _, err := buildMessage(msgOpts{
		From: "a@b.com", To: []string{"c@d.com"}, Body: "x", MessageID: "<x@y>",
		Headers: map[string]string{"X-Zeta": "1", "X-Alpha": "2"},
	})
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	s := string(raw)
	ai := strings.Index(s, "X-Alpha: 2")
	zi := strings.Index(s, "X-Zeta: 1")
	if ai < 0 || zi < 0 || ai > zi {
		t.Errorf("headers not sorted deterministically: %s", s)
	}
}

func TestGenerateMessageIDUsesFromDomain(t *testing.T) {
	id := generateMessageID("bot@example.com")
	if !strings.HasSuffix(id, "@example.com>") {
		t.Errorf("message id domain: got %q", id)
	}
	if !strings.HasPrefix(id, "<") {
		t.Errorf("message id should be bracketed: got %q", id)
	}
}

// --- IMAP: readIMAPLine / literal handling ---

func TestReadIMAPLineNoLiteral(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("a0001 OK LOGIN completed\r\n"))
	line, lits, err := readIMAPLine(r)
	if err != nil {
		t.Fatalf("readIMAPLine: %v", err)
	}
	if line != "a0001 OK LOGIN completed" {
		t.Errorf("line: got %q", line)
	}
	if len(lits) != 0 {
		t.Errorf("expected no literals, got %v", lits)
	}
}

func TestReadIMAPLineOneLiteral(t *testing.T) {
	// A literal containing an embedded CRLF, followed by the closing paren.
	content := "hi\r\n\r\n"
	raw := "* 1 FETCH (BODY[TEXT] {" + strconv.Itoa(len(content)) + "}\r\n" + content + ")\r\n"
	r := bufio.NewReader(strings.NewReader(raw))
	line, lits, err := readIMAPLine(r)
	if err != nil {
		t.Fatalf("readIMAPLine: %v", err)
	}
	if len(lits) != 1 {
		t.Fatalf("expected 1 literal, got %d: %v", len(lits), lits)
	}
	got := line[lits[0].Start : lits[0].Start+lits[0].Len]
	if got != content {
		t.Errorf("literal content: got %q", got)
	}
	if !strings.HasSuffix(line, ")") {
		t.Errorf("expected line to end with closing paren, got %q", line)
	}
}

func TestReadIMAPLineTwoLiterals(t *testing.T) {
	headerBlock := "From: a@b.com\r\nSubject: hi\r\n\r\n"
	body := "hello world"
	raw := "* 1 FETCH (UID 5 BODY[HEADER.FIELDS (FROM SUBJECT)] {" +
		strconv.Itoa(len(headerBlock)) + "}\r\n" + headerBlock +
		" BODY[TEXT] {" + strconv.Itoa(len(body)) + "}\r\n" + body + ")\r\n"
	r := bufio.NewReader(strings.NewReader(raw))
	line, lits, err := readIMAPLine(r)
	if err != nil {
		t.Fatalf("readIMAPLine: %v", err)
	}
	if len(lits) != 2 {
		t.Fatalf("expected 2 literals, got %d", len(lits))
	}
	gotHeaders := line[lits[0].Start : lits[0].Start+lits[0].Len]
	gotBody := line[lits[1].Start : lits[1].Start+lits[1].Len]
	if gotHeaders != headerBlock {
		t.Errorf("header literal: got %q want %q", gotHeaders, headerBlock)
	}
	if gotBody != body {
		t.Errorf("body literal: got %q want %q", gotBody, body)
	}
}

// --- IMAP: SEARCH parsing ---

func TestParseSearchLine(t *testing.T) {
	cases := []struct {
		line string
		want []uint32
	}{
		{"* SEARCH 1 2 3", []uint32{1, 2, 3}},
		{"* SEARCH", nil},
		{"* SEARCH 42", []uint32{42}},
		{"* FLAGS (\\Seen)", nil},
	}
	for _, c := range cases {
		got := parseSearchLine(c.line)
		if len(got) != len(c.want) {
			t.Errorf("parseSearchLine(%q): got %v want %v", c.line, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseSearchLine(%q): got %v want %v", c.line, got, c.want)
			}
		}
	}
}

// --- IMAP: FETCH parsing ---

func TestParseFetchLine(t *testing.T) {
	headerBlock := "From: alice@example.com\r\nTo: bot@example.com\r\nSubject: Hello\r\nDate: Mon, 2 Jan 2026 03:04:05 +0000\r\nMessage-ID: <abc@example.com>\r\n\r\n"
	body := "the message body"
	text := "* 1 FETCH (UID 7 BODY[HEADER.FIELDS (FROM TO SUBJECT DATE MESSAGE-ID)] {" +
		strconv.Itoa(len(headerBlock)) + "}\r\n" + headerBlock +
		" BODY[TEXT] {" + strconv.Itoa(len(body)) + "}\r\n" + body + ")"
	literals := []literalSpan{
		{Start: strings.Index(text, headerBlock), Len: len(headerBlock)},
		{Start: strings.Index(text, body), Len: len(body)},
	}
	headers, gotBody, err := parseFetchLine(text, literals)
	if err != nil {
		t.Fatalf("parseFetchLine: %v", err)
	}
	if headers["from"] != "alice@example.com" {
		t.Errorf("from: got %q", headers["from"])
	}
	if headers["subject"] != "Hello" {
		t.Errorf("subject: got %q", headers["subject"])
	}
	if headers["message-id"] != "<abc@example.com>" {
		t.Errorf("message-id: got %q", headers["message-id"])
	}
	if gotBody != body {
		t.Errorf("body: got %q want %q", gotBody, body)
	}
}

func TestParseFetchLineMissingLiterals(t *testing.T) {
	if _, _, err := parseFetchLine("* 1 FETCH (UID 1)", nil); err == nil {
		t.Fatal("expected error with no literals")
	}
}

func TestParseHeaderBlockUnfoldsContinuations(t *testing.T) {
	block := "Subject: line one\r\n continued\r\nFrom: a@b.com\r\n\r\n"
	headers := parseHeaderBlock(block)
	if headers["subject"] != "line one continued" {
		t.Errorf("subject: got %q", headers["subject"])
	}
	if headers["from"] != "a@b.com" {
		t.Errorf("from: got %q", headers["from"])
	}
}

// --- IMAP: tagged-response reader ---

func TestReadUntilTagged(t *testing.T) {
	raw := "* SEARCH 1 2 3\r\na0001 OK SEARCH completed\r\n"
	c := newIMAPClient(nil)
	c.r = bufio.NewReader(strings.NewReader(raw))
	untagged, _, status, err := c.readUntilTagged("a0001")
	if err != nil {
		t.Fatalf("readUntilTagged: %v", err)
	}
	if status != "OK" {
		t.Errorf("status: got %q", status)
	}
	if len(untagged) != 1 || untagged[0] != "* SEARCH 1 2 3" {
		t.Errorf("untagged: got %v", untagged)
	}
}

func TestReadUntilTaggedFailureStatus(t *testing.T) {
	raw := "a0002 NO LOGIN failed\r\n"
	c := newIMAPClient(nil)
	c.r = bufio.NewReader(strings.NewReader(raw))
	_, _, status, err := c.readUntilTagged("a0002")
	if err != nil {
		t.Fatalf("readUntilTagged: %v", err)
	}
	if status != "NO" {
		t.Errorf("status: got %q", status)
	}
}

// --- messageEvent: emitted context/filter shape + dedup ---

func TestMessageEvent(t *testing.T) {
	headers := map[string]string{
		"from": "alice@example.com", "to": "bot@example.com",
		"subject": "Hello", "date": "Mon, 2 Jan 2026 03:04:05 +0000",
		"message-id": "<abc@example.com>",
	}
	ev := messageEvent("INBOX", 7, headers, "body text")
	if ev["event"] != "message" || ev["kind"] != "message" {
		t.Fatalf("event/kind: %v", ev)
	}
	if ev["dedup"] != "<abc@example.com>" {
		t.Errorf("dedup should be the Message-ID: got %v", ev["dedup"])
	}
	ctx, ok := ev["context"].(map[string]any)
	if !ok {
		t.Fatalf("context missing or wrong type: %v", ev["context"])
	}
	for _, k := range []string{"from", "to", "subject", "date", "message_id", "body", "uid", "mailbox", "froms", "subjects"} {
		if _, ok := ctx[k]; !ok {
			t.Errorf("context missing key %q: %v", k, ctx)
		}
	}
	if ctx["from"] != "alice@example.com" || ctx["froms"] != "alice@example.com" {
		t.Errorf("from/froms alias mismatch: %v", ctx)
	}
	if ctx["uid"] != uint32(7) {
		t.Errorf("uid: got %v", ctx["uid"])
	}
}

func TestMessageEventFallsBackToUIDDedupWhenNoMessageID(t *testing.T) {
	ev := messageEvent("INBOX", 42, map[string]string{"from": "a@b.com"}, "")
	if ev["dedup"] != "uid:42@INBOX" {
		t.Errorf("dedup fallback: got %v", ev["dedup"])
	}
}

// --- Describe() ---

func TestDescribe(t *testing.T) {
	d := emailPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "email" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if _, ok := d.Connection["smtp"]; !ok {
		t.Error("missing smtp connection field")
	}
	if _, ok := d.Connection["imap"]; !ok {
		t.Error("missing imap connection field")
	}
	verbs := map[string]bool{}
	for _, v := range d.Verbs {
		verbs[v.Name] = true
	}
	for _, want := range []string{"send", "send_raw"} {
		if !verbs[want] {
			t.Errorf("missing verb %q", want)
		}
	}
	if len(d.Events) != 1 || d.Events[0].Name != "message" {
		t.Fatalf("events: got %#v", d.Events)
	}
	ev := d.Events[0]
	for _, k := range []string{"froms", "from", "subjects", "subject"} {
		if _, ok := ev.Filters[k]; !ok {
			t.Errorf("event missing filter %q", k)
		}
	}
	for _, k := range []string{"from", "to", "subject", "date", "message_id", "body", "uid", "mailbox"} {
		if _, ok := ev.Context[k]; !ok {
			t.Errorf("event missing context key %q", k)
		}
	}
	if len(d.Capabilities.Egress) != 0 {
		t.Errorf("egress should be operator-specific (empty by default): %v", d.Capabilities.Egress)
	}
}

// --- connection parsing defaults ---

func TestParseSMTPConnDefaults(t *testing.T) {
	c, err := parseSMTPConn(map[string]any{"host": "smtp.example.com"})
	if err != nil {
		t.Fatalf("parseSMTPConn: %v", err)
	}
	if c.Port != 587 {
		t.Errorf("default port: got %d", c.Port)
	}
	if c.TLS != "starttls" {
		t.Errorf("default tls: got %q", c.TLS)
	}
}

func TestParseSMTPConnRejectsBadTLS(t *testing.T) {
	if _, err := parseSMTPConn(map[string]any{"host": "h", "tls": "bogus"}); err == nil {
		t.Fatal("expected error for invalid tls mode")
	}
}

func TestParseIMAPConnDefaults(t *testing.T) {
	c, err := parseIMAPConn(map[string]any{"host": "imap.example.com"})
	if err != nil {
		t.Fatalf("parseIMAPConn: %v", err)
	}
	if c.Port != 993 {
		t.Errorf("default port: got %d", c.Port)
	}
	if c.Mailbox != "INBOX" {
		t.Errorf("default mailbox: got %q", c.Mailbox)
	}
	if !c.TLS {
		t.Error("default tls should be true")
	}
	if !c.MarkSeen {
		t.Error("default mark_seen should be true")
	}
	if c.Search != "UNSEEN" {
		t.Errorf("default search: got %q", c.Search)
	}
	if c.PollInterval != 60*time.Second {
		t.Errorf("default poll_interval: got %v", c.PollInterval)
	}
}

func TestParseIMAPConnOverrides(t *testing.T) {
	c, err := parseIMAPConn(map[string]any{
		"host": "imap.example.com", "port": 143, "mailbox": "Work",
		"tls": false, "poll_interval": "10s", "mark_seen": false, "search": "ALL",
	})
	if err != nil {
		t.Fatalf("parseIMAPConn: %v", err)
	}
	if c.Port != 143 || c.Mailbox != "Work" || c.TLS || c.MarkSeen || c.Search != "ALL" {
		t.Errorf("overrides not applied: %+v", c)
	}
	if c.PollInterval != 10*time.Second {
		t.Errorf("poll_interval: got %v", c.PollInterval)
	}
}
