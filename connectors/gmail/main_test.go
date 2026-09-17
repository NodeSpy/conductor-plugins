package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn builds a connection map with the managed-OAuth2 token already
// injected (as the daemon would do), pointed at srv via api_base.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{
		plugin.AccessTokenKey: "test-token",
		"api_base":            srv.URL,
	}
}

func checkBearer(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization header: got %q want %q", got, "Bearer test-token")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestMessagesHoistsList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/messages" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("q") != "is:unread" || q.Get("maxResults") != "10" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{
			"messages":           []any{map[string]any{"id": "m1"}, map[string]any{"id": "m2"}},
			"nextPageToken":      "abc",
			"resultSizeEstimate": 2,
		})
	}))
	defer srv.Close()

	p := newGmailPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "messages", Connection: testConn(srv),
		Options: map[string]any{"q": "is:unread", "maxResults": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	if items[0].(map[string]any)["id"] != "m1" {
		t.Errorf("items[0]: %#v", items[0])
	}
}

func TestMessagesLabelIdsRepeated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query()["labelIds"]
		if len(got) != 2 || got[0] != "INBOX" || got[1] != "UNREAD" {
			t.Errorf("labelIds: got %v", got)
		}
		writeJSON(w, 200, map[string]any{"messages": []any{}})
	}))
	defer srv.Close()

	p := newGmailPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "messages", Connection: testConn(srv),
		Options: map[string]any{"labelIds": []any{"INBOX", "UNREAD"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMessageGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/messages/m1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if r.URL.Query().Get("format") != "metadata" {
			t.Errorf("format: got %q", r.URL.Query().Get("format"))
		}
		writeJSON(w, 200, map[string]any{"id": "m1", "snippet": "hello"})
	}))
	defer srv.Close()

	p := newGmailPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "message_get", Connection: testConn(srv),
		Options: map[string]any{"message_id": "m1", "format": "metadata"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "m1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

// TestSendBuildsRawMIME asserts the connector builds a valid RFC 5322
// message, base64url-encodes it into `raw`, and POSTs it to /messages/send.
// The test decodes `raw` itself and asserts on the decoded MIME text so the
// encoding step is genuinely exercised, not just assumed.
func TestSendBuildsRawMIME(t *testing.T) {
	var gotRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/messages/send" {
			t.Errorf("request: got %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotRaw, _ = body["raw"].(string)
		writeJSON(w, 200, map[string]any{"id": "sent-1"})
	}))
	defer srv.Close()

	p := newGmailPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "send", Connection: testConn(srv),
		Options: map[string]any{
			"to":      []any{"alice@example.com", "bob@example.com"},
			"cc":      []any{"carol@example.com"},
			"subject": "Hello there",
			"text":    "plain body",
			"html":    "<p>html body</p>",
			"from":    "me@example.com",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "sent-1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	if gotRaw == "" {
		t.Fatal("expected a non-empty raw field")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(gotRaw)
	if err != nil {
		t.Fatalf("raw is not valid base64url: %v", err)
	}
	msg := string(decoded)
	for _, want := range []string{
		"To: alice@example.com, bob@example.com",
		"Cc: carol@example.com",
		"Subject: Hello there",
		"From: me@example.com",
		"plain body",
		"<p>html body</p>",
		"multipart/alternative",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("decoded raw message missing %q; got:\n%s", want, msg)
		}
	}
}

func TestSendTextOnlyIsSinglePart(t *testing.T) {
	var gotRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotRaw, _ = body["raw"].(string)
		writeJSON(w, 200, map[string]any{"id": "sent-2"})
	}))
	defer srv.Close()

	p := newGmailPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "send", Connection: testConn(srv),
		Options: map[string]any{
			"to":      []any{"alice@example.com"},
			"subject": "Just text",
			"text":    "only plain text",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(gotRaw)
	if err != nil {
		t.Fatal(err)
	}
	msg := string(decoded)
	if strings.Contains(msg, "multipart/alternative") {
		t.Errorf("text-only send should not be multipart, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Content-Type: text/plain") {
		t.Errorf("expected text/plain content type, got:\n%s", msg)
	}
}

func TestSendMissingRequiredFields(t *testing.T) {
	p := newGmailPlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t"}

	cases := []struct {
		name string
		opts map[string]any
	}{
		{"missing to", map[string]any{"subject": "s", "text": "t"}},
		{"missing subject", map[string]any{"to": []any{"a@b.com"}, "text": "t"}},
		{"missing body", map[string]any{"to": []any{"a@b.com"}, "subject": "s"}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: "send", Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("%s: expected error", tc.name)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s: expected CodeInvalidParams, got %v", tc.name, err)
		}
	}
}

func TestLabelsAndLabelCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/labels":
			writeJSON(w, 200, map[string]any{"labels": []any{map[string]any{"id": "l1", "name": "Work"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/labels":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "Work" || body["labelListVisibility"] != "labelShow" {
				t.Errorf("label_create body: %#v", body)
			}
			writeJSON(w, 200, map[string]any{"id": "l1", "name": "Work"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newGmailPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "labels", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["name"] != "Work" {
		t.Fatalf("labels items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "label_create", Connection: testConn(srv),
		Options: map[string]any{"name": "Work", "labelListVisibility": "labelShow"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "l1" {
		t.Fatalf("label_create result: %#v", res.Outputs["result"])
	}
}

func TestDraftsAndDraftCreate(t *testing.T) {
	var draftBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/drafts":
			writeJSON(w, 200, map[string]any{"drafts": []any{map[string]any{"id": "d1"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/drafts":
			_ = json.NewDecoder(r.Body).Decode(&draftBody)
			writeJSON(w, 200, map[string]any{"id": "d1"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newGmailPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "drafts", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("drafts items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "draft_create", Connection: testConn(srv),
		Options: map[string]any{"to": []any{"a@b.com"}, "subject": "s", "text": "body"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("draft_create result: %#v", res.Outputs["result"])
	}
	message, ok := draftBody["message"].(map[string]any)
	if !ok || message["raw"] == "" || message["raw"] == nil {
		t.Fatalf("draft_create body.message.raw: %#v", draftBody)
	}
}

func TestThreadsAndThreadGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch r.URL.Path {
		case "/threads":
			if r.URL.Query().Get("q") != "in:inbox" {
				t.Errorf("q: got %q", r.URL.Query().Get("q"))
			}
			writeJSON(w, 200, map[string]any{"threads": []any{map[string]any{"id": "t1"}}})
		case "/threads/t1":
			writeJSON(w, 200, map[string]any{"id": "t1", "messages": []any{}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newGmailPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "threads", Connection: testConn(srv),
		Options: map[string]any{"q": "in:inbox"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("threads items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "thread_get", Connection: testConn(srv),
		Options: map[string]any{"thread_id": "t1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "t1" {
		t.Fatalf("thread_get result: %#v", res.Outputs["result"])
	}
}

func TestModifySendsAddAndRemoveLabelIds(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/messages/m1/modify" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, 200, map[string]any{"id": "m1"})
	}))
	defer srv.Close()

	p := newGmailPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "modify", Connection: testConn(srv),
		Options: map[string]any{
			"message_id":    "m1",
			"add_labels":    []any{"STARRED"},
			"remove_labels": []any{"UNREAD", "INBOX"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	addLabelIds, ok := gotBody["addLabelIds"].([]any)
	if !ok || len(addLabelIds) != 1 || addLabelIds[0] != "STARRED" {
		t.Errorf("addLabelIds: %#v", gotBody["addLabelIds"])
	}
	removeLabelIds, ok := gotBody["removeLabelIds"].([]any)
	if !ok || len(removeLabelIds) != 2 {
		t.Errorf("removeLabelIds: %#v", gotBody["removeLabelIds"])
	}
}

func TestTrashAndUntrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch r.URL.Path {
		case "/messages/m1/trash":
			if r.Method != http.MethodPost {
				t.Errorf("trash method: got %s", r.Method)
			}
			writeJSON(w, 200, map[string]any{"id": "m1"})
		case "/messages/m1/untrash":
			if r.Method != http.MethodPost {
				t.Errorf("untrash method: got %s", r.Method)
			}
			writeJSON(w, 200, map[string]any{"id": "m1"})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newGmailPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "trash", Connection: testConn(srv), Options: map[string]any{"message_id": "m1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "untrash", Connection: testConn(srv), Options: map[string]any{"message_id": "m1"}}); err != nil {
		t.Fatal(err)
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/messages" && r.URL.Query().Get("q") == "raw":
			writeJSON(w, 200, map[string]any{"messages": []any{map[string]any{"id": "m9"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/labels":
			writeJSON(w, 200, map[string]any{"id": "new-l"})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newGmailPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/messages", "query": map[string]any{"q": "raw"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := result["messages"]; !ok {
		t.Fatalf("result missing messages: %#v", result)
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "POST", "path": "labels", "body": map[string]any{"name": "New"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatchArrayResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/foo" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{map[string]any{"a": 1}})
	}))
	defer srv.Close()

	p := newGmailPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/foo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid_token"}}`))
	}))
	defer srv.Close()

	p := newGmailPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "labels", Connection: testConn(srv)})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok {
		t.Fatalf("expected *plugin.Error, got %T: %v", err, err)
	}
	if pe.Code != plugin.CodeInternalError {
		t.Errorf("code: got %d want %d", pe.Code, plugin.CodeInternalError)
	}
	if !containsAll(pe.Message, "401", "invalid_token") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingAccessTokenIsInvalidParams(t *testing.T) {
	p := newGmailPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "labels",
		Connection: map[string]any{}, // no access_token injected
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing access token, got %v", err)
	}
	if !containsAll(pe.Message, "access token", "conductor connector auth gmail") {
		t.Errorf("message should point at the auth: block / login flow, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newGmailPlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"message_get", map[string]any{}},
		{"label_create", map[string]any{}},
		{"thread_get", map[string]any{}},
		{"modify", map[string]any{}},
		{"trash", map[string]any{}},
		{"untrash", map[string]any{}},
		{"api", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("%s: expected error for missing required options", tc.verb)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s: expected CodeInvalidParams, got %v", tc.verb, err)
		}
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newGmailPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{plugin.AccessTokenKey: "t"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for unknown verb, got %v", err)
	}
}

func TestDescribe(t *testing.T) {
	p := newGmailPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "gmail" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["api_base"].Type != "string" || d.Connection["api_base"].Required {
		t.Errorf("connection.api_base: %#v", d.Connection["api_base"])
	}

	want := []string{
		"messages", "message_get", "send", "labels", "label_create",
		"drafts", "draft_create", "threads", "thread_get", "modify",
		"trash", "untrash", "api",
	}
	got := map[string]bool{}
	for _, v := range d.Verbs {
		got[v.Name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("Describe missing verb %q", w)
		}
	}
	if len(d.Verbs) != len(want) {
		t.Errorf("verb count: got %d want %d", len(d.Verbs), len(want))
	}

	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "gmail.googleapis.com:443" {
		t.Errorf("egress: got %v want [gmail.googleapis.com:443]", d.Capabilities.Egress)
	}

	if d.Auth == nil {
		t.Fatal("expected Describe().Auth to be non-nil (managed OAuth2)")
	}
	if d.Auth.TokenURL != "https://oauth2.googleapis.com/token" {
		t.Errorf("Auth.TokenURL: got %q", d.Auth.TokenURL)
	}
	if d.Auth.AuthURL != "https://accounts.google.com/o/oauth2/v2/auth" {
		t.Errorf("Auth.AuthURL: got %q", d.Auth.AuthURL)
	}
	foundScope := false
	for _, s := range d.Auth.Scopes {
		if s == "https://www.googleapis.com/auth/gmail.modify" {
			foundScope = true
		}
	}
	if !foundScope {
		t.Errorf("Auth.Scopes missing gmail.modify: %v", d.Auth.Scopes)
	}
	if d.Auth.AuthParams["access_type"] != "offline" {
		t.Errorf("Auth.AuthParams.access_type: got %q want %q", d.Auth.AuthParams["access_type"], "offline")
	}
	if d.Auth.AuthParams["prompt"] != "consent" {
		t.Errorf("Auth.AuthParams.prompt: got %q want %q", d.Auth.AuthParams["prompt"], "consent")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
