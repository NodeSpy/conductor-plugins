package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// --- X-Twilio-Signature validation: the important test ---

// signTwilio computes the reference signature by hand (independent of
// verifyTwilioSignature's implementation) so the test proves the algorithm,
// not just that the code agrees with itself.
func signTwilio(t *testing.T, authToken, requestURL string, form url.Values) string {
	t.Helper()
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	data := requestURL
	for _, k := range keys {
		data += k + form.Get(k)
	}
	mac := hmac.New(sha1.New, []byte(authToken))
	mac.Write([]byte(data))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func TestVerifyTwilioSignature(t *testing.T) {
	authToken := "s3cret-auth-token"
	requestURL := "https://example.com/twilio"
	form := url.Values{
		"From": {"+15551234567"}, "To": {"+15557654321"},
		"Body": {"hello"}, "MessageSid": {"SM123"},
	}
	good := signTwilio(t, authToken, requestURL, form)

	if !verifyTwilioSignature(authToken, requestURL, form, good) {
		t.Fatal("valid signature rejected")
	}

	// Tampered signature.
	if verifyTwilioSignature(authToken, requestURL, form, good+"x") {
		t.Fatal("tampered signature accepted")
	}
	if verifyTwilioSignature(authToken, requestURL, form, "") {
		t.Fatal("empty signature accepted")
	}

	// Tampered param — a real MITM changing the payload without resigning.
	tampered := url.Values{}
	for k, v := range form {
		tampered[k] = append([]string(nil), v...)
	}
	tampered.Set("Body", "goodbye")
	if verifyTwilioSignature(authToken, requestURL, tampered, good) {
		t.Fatal("signature for a different body accepted")
	}

	// Tampered URL (e.g. public_url misconfigured or a proxy rewrote path).
	if verifyTwilioSignature(authToken, requestURL+"/other", form, good) {
		t.Fatal("signature valid for a different URL accepted")
	}

	// Wrong key.
	if verifyTwilioSignature("wrong-token", requestURL, form, good) {
		t.Fatal("signature valid under the wrong auth token accepted")
	}
}

// TestRequireSignatureConfig proves the fail-closed contract: validate=true
// needs both auth_token and public_url, and validate=false always passes.
func TestRequireSignatureConfig(t *testing.T) {
	if err := requireSignatureConfig(true, "", ""); err == nil {
		t.Fatal("expected error: neither auth_token nor public_url set")
	}
	if err := requireSignatureConfig(true, "tok", ""); err == nil {
		t.Fatal("expected error: no public_url")
	}
	if err := requireSignatureConfig(true, "", "https://example.com"); err == nil {
		t.Fatal("expected error: no auth_token")
	}
	if err := requireSignatureConfig(true, "tok", "https://example.com"); err != nil {
		t.Fatalf("both set: unexpected error: %v", err)
	}
	if err := requireSignatureConfig(false, "", ""); err != nil {
		t.Fatalf("validate false: unexpected error: %v", err)
	}
}

// --- inbound webhook parsing: sms/call context + filters + dedup ---

func TestTwilioEventSMS(t *testing.T) {
	form := url.Values{
		"From": {"+15551234567"}, "To": {"+15557654321"},
		"Body": {"hi there"}, "MessageSid": {"SM123"}, "NumMedia": {"2"},
	}
	ev, dk := twilioEvent(form)
	if ev == nil {
		t.Fatal("expected an sms event")
	}
	if dk != "SM123" {
		t.Fatalf("dedup key: got %q want SM123", dk)
	}
	if ev["event"] != "sms" || ev["kind"] != "sms" {
		t.Fatalf("event/kind: %#v", ev)
	}
	ctx, ok := ev["context"].(map[string]any)
	if !ok {
		t.Fatalf("context: %#v", ev["context"])
	}
	want := map[string]any{
		"from": "+15551234567", "to": "+15557654321", "body": "hi there",
		"message_sid": "SM123", "num_media": 2,
		"froms": "+15551234567", "tos": "+15557654321",
	}
	for k, v := range want {
		if ctx[k] != v {
			t.Errorf("context[%q] = %#v, want %#v", k, ctx[k], v)
		}
	}
}

// SmsSid is the alternate field name Twilio uses on some SMS callbacks; the
// dedup/message_sid logic must fall back to it when MessageSid is absent.
func TestTwilioEventSMSSmsSidFallback(t *testing.T) {
	form := url.Values{"From": {"+1"}, "To": {"+2"}, "Body": {"x"}, "SmsSid": {"SM999"}}
	ev, dk := twilioEvent(form)
	if ev == nil || dk != "SM999" {
		t.Fatalf("event/dedup: %#v %q", ev, dk)
	}
	ctx := ev["context"].(map[string]any)
	if ctx["message_sid"] != "SM999" {
		t.Fatalf("message_sid: %#v", ctx["message_sid"])
	}
}

func TestTwilioEventCall(t *testing.T) {
	form := url.Values{
		"From": {"+15551234567"}, "To": {"+15557654321"},
		"CallSid": {"CA123"}, "CallStatus": {"ringing"},
	}
	ev, dk := twilioEvent(form)
	if ev == nil {
		t.Fatal("expected a call event")
	}
	if dk != "CA123" {
		t.Fatalf("dedup key: got %q want CA123", dk)
	}
	if ev["event"] != "call" {
		t.Fatalf("event: %#v", ev["event"])
	}
	ctx := ev["context"].(map[string]any)
	want := map[string]any{
		"from": "+15551234567", "to": "+15557654321",
		"call_sid": "CA123", "call_status": "ringing",
		"froms": "+15551234567", "tos": "+15557654321",
	}
	for k, v := range want {
		if ctx[k] != v {
			t.Errorf("context[%q] = %#v, want %#v", k, ctx[k], v)
		}
	}
}

func TestTwilioEventNeitherShape(t *testing.T) {
	ev, dk := twilioEvent(url.Values{"Unrelated": {"x"}})
	if ev != nil || dk != "" {
		t.Fatalf("expected no event, got %#v %q", ev, dk)
	}
}

// --- verb call-building: pure, hermetic ---

func TestVerbCall(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		opts       map[string]any
		wantMethod string
		wantPath   string
		wantParams url.Values
	}{
		{
			name:       "send_sms",
			verb:       "send_sms",
			opts:       map[string]any{"from": "+1", "to": "+2", "body": "hi", "media_url": []any{"https://x/1.png"}},
			wantMethod: http.MethodPost, wantPath: "Messages.json",
			wantParams: url.Values{"From": {"+1"}, "To": {"+2"}, "Body": {"hi"}, "MediaUrl": {"https://x/1.png"}},
		},
		{
			name:       "send_whatsapp adds prefix",
			verb:       "send_whatsapp",
			opts:       map[string]any{"from": "+1", "to": "+2", "body": "hi"},
			wantMethod: http.MethodPost, wantPath: "Messages.json",
			wantParams: url.Values{"From": {"whatsapp:+1"}, "To": {"whatsapp:+2"}, "Body": {"hi"}},
		},
		{
			name:       "send_whatsapp keeps existing prefix",
			verb:       "send_whatsapp",
			opts:       map[string]any{"from": "whatsapp:+1", "to": "+2", "body": "hi"},
			wantMethod: http.MethodPost, wantPath: "Messages.json",
			wantParams: url.Values{"From": {"whatsapp:+1"}, "To": {"whatsapp:+2"}, "Body": {"hi"}},
		},
		{
			name:       "get_message",
			verb:       "get_message",
			opts:       map[string]any{"sid": "SM1"},
			wantMethod: http.MethodGet, wantPath: "Messages/SM1.json",
		},
		{
			name:       "list_messages",
			verb:       "list_messages",
			opts:       map[string]any{"to": "+2", "page_size": 10},
			wantMethod: http.MethodGet, wantPath: "Messages.json",
			wantParams: url.Values{"To": {"+2"}, "PageSize": {"10"}},
		},
		{
			name:       "make_call with url",
			verb:       "make_call",
			opts:       map[string]any{"from": "+1", "to": "+2", "url": "https://x/twiml"},
			wantMethod: http.MethodPost, wantPath: "Calls.json",
			wantParams: url.Values{"From": {"+1"}, "To": {"+2"}, "Url": {"https://x/twiml"}},
		},
		{
			name:       "make_call with inline twiml",
			verb:       "make_call",
			opts:       map[string]any{"from": "+1", "to": "+2", "twiml": "<Response/>"},
			wantMethod: http.MethodPost, wantPath: "Calls.json",
			wantParams: url.Values{"From": {"+1"}, "To": {"+2"}, "Twiml": {"<Response/>"}},
		},
		{
			name:       "get_call",
			verb:       "get_call",
			opts:       map[string]any{"sid": "CA1"},
			wantMethod: http.MethodGet, wantPath: "Calls/CA1.json",
		},
		{
			name:       "api escape hatch",
			verb:       "api",
			opts:       map[string]any{"method": "get", "path": "/Usage.json", "params": map[string]any{"Category": "sms"}},
			wantMethod: http.MethodGet, wantPath: "Usage.json",
			wantParams: url.Values{"Category": {"sms"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := verbCall(tc.verb, tc.opts)
			if err != nil {
				t.Fatalf("verbCall(%s): unexpected error: %v", tc.verb, err)
			}
			if got.method != tc.wantMethod || got.path != tc.wantPath {
				t.Fatalf("verbCall(%s) = %q %q, want %q %q", tc.verb, got.method, got.path, tc.wantMethod, tc.wantPath)
			}
			if tc.wantParams != nil && !reflect.DeepEqual(got.params, tc.wantParams) {
				t.Fatalf("verbCall(%s) params = %#v, want %#v", tc.verb, got.params, tc.wantParams)
			}
		})
	}
}

func TestVerbCallErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"send_sms", map[string]any{"to": "+2", "body": "hi"}},                            // no from
		{"send_sms", map[string]any{"from": "+1", "body": "hi"}},                          // no to
		{"send_sms", map[string]any{"from": "+1", "to": "+2"}},                            // no body
		{"get_message", map[string]any{}},                                                 // no sid
		{"make_call", map[string]any{"from": "+1", "to": "+2"}},                           // neither url nor twiml
		{"make_call", map[string]any{"from": "+1", "to": "+2", "url": "a", "twiml": "b"}}, // both
		{"get_call", map[string]any{}},                                                    // no sid
		{"api", map[string]any{"path": "x"}},                                              // no method
		{"api", map[string]any{"method": "GET"}},                                          // no path
		{"nope", map[string]any{}},                                                        // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbCall(tc.verb, tc.opts); err == nil {
			t.Errorf("verbCall(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// --- HTTP verb execution against a fake Twilio server ---

func TestDoSendSMS(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool
	var gotBody string
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sid":"SM123","status":"queued"}`))
	}))
	defer srv.Close()

	conn := twilioConn{accountSID: "AC1", authToken: "tok", base: srv.URL}
	res, err := twilioPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "send_sms",
		Connection: map[string]any{"account_sid": "AC1", "auth_token": "tok", "api_base": srv.URL},
		Options:    map[string]any{"from": "+1", "to": "+2", "body": "hi"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !gotOK || gotUser != "AC1" || gotPass != "tok" {
		t.Fatalf("basic auth: user=%q pass=%q ok=%v", gotUser, gotPass, gotOK)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Fatalf("content-type: %q", gotContentType)
	}
	form, err := url.ParseQuery(gotBody)
	if err != nil {
		t.Fatalf("parsing sent body: %v", err)
	}
	if form.Get("From") != "+1" || form.Get("To") != "+2" || form.Get("Body") != "hi" {
		t.Fatalf("sent form: %#v", form)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["sid"] != "SM123" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if res.Outputs["status_code"] != http.StatusCreated {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
	_ = conn // conn is exercised via Invoke's own parseConn; kept for clarity
}

func TestDoListMessagesHoistsResultsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("list_messages should GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[{"sid":"SM1"},{"sid":"SM2"}]}`))
	}))
	defer srv.Close()

	res, err := twilioPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "list_messages",
		Connection: map[string]any{"account_sid": "AC1", "auth_token": "tok", "api_base": srv.URL},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	msgs, ok := res.Outputs["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages: %#v", res.Outputs["messages"])
	}
}

func TestDoNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Authenticate"}`))
	}))
	defer srv.Close()

	_, err := twilioPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "get_message",
		Connection: map[string]any{"account_sid": "AC1", "auth_token": "tok", "api_base": srv.URL},
		Options:    map[string]any{"sid": "SM1"},
	})
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "Authenticate") {
		t.Fatalf("error should surface the response body: %v", pe.Message)
	}
}

func TestInvokeMissingConnection(t *testing.T) {
	_, err := twilioPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "send_sms",
		Connection: map[string]any{"auth_token": "tok"},
		Options:    map[string]any{"from": "+1", "to": "+2", "body": "hi"},
	})
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing account_sid, got %v", err)
	}
}

// --- Describe ---

func TestDescribe(t *testing.T) {
	d := twilioPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "twilio" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Egress, "api.twilio.com:443") {
		t.Fatalf("capabilities.egress: %#v", d.Capabilities.Egress)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("twilio spawns nothing: %#v", d.Capabilities)
	}
	wantVerbs := []string{"send_sms", "send_whatsapp", "get_message", "list_messages", "make_call", "get_call", "api"}
	got := map[string]bool{}
	for _, v := range d.Verbs {
		got[v.Name] = true
	}
	for _, w := range wantVerbs {
		if !got[w] {
			t.Errorf("Describe missing verb %q", w)
		}
	}
	if len(d.Verbs) != len(wantVerbs) {
		t.Errorf("verb count: got %d want %d", len(d.Verbs), len(wantVerbs))
	}
	wantEvents := []string{"sms", "call"}
	gotEv := map[string]bool{}
	for _, e := range d.Events {
		gotEv[e.Name] = true
	}
	for _, w := range wantEvents {
		if !gotEv[w] {
			t.Errorf("Describe missing event %q", w)
		}
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// asPluginError unwraps a *plugin.Error without needing errors.As just for
// the test (the SDK returns the concrete type directly).
func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}
