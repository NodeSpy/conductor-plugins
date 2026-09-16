package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// --- verb call-building: pure, hermetic ---

func TestVerbCall(t *testing.T) {
	cases := []struct {
		name         string
		verb         string
		opts         map[string]any
		wantMethod   string
		wantPath     string
		wantWithUser bool
		wantParams   url.Values
	}{
		{
			name:       "send minimal",
			verb:       "send",
			opts:       map[string]any{"message": "hi"},
			wantMethod: http.MethodPost, wantPath: "messages.json", wantWithUser: true,
			wantParams: url.Values{"message": {"hi"}},
		},
		{
			name: "send full non-emergency",
			verb: "send",
			opts: map[string]any{
				"message": "hi", "title": "t", "priority": 1, "url": "https://x", "url_title": "link",
				"sound": "cosmic", "device": "phone", "html": true, "monospace": false,
				"timestamp": 123, "tags": "a,b",
			},
			wantMethod: http.MethodPost, wantPath: "messages.json", wantWithUser: true,
			wantParams: url.Values{
				"message": {"hi"}, "title": {"t"}, "priority": {"1"}, "url": {"https://x"},
				"url_title": {"link"}, "sound": {"cosmic"}, "device": {"phone"}, "html": {"1"},
				"timestamp": {"123"}, "tags": {"a,b"},
			},
		},
		{
			name:       "send priority 2 with retry+expire",
			verb:       "send",
			opts:       map[string]any{"message": "help", "priority": 2, "retry": 30, "expire": 3600},
			wantMethod: http.MethodPost, wantPath: "messages.json", wantWithUser: true,
			wantParams: url.Values{"message": {"help"}, "priority": {"2"}, "retry": {"30"}, "expire": {"3600"}},
		},
		{
			name:       "validate_user",
			verb:       "validate_user",
			opts:       map[string]any{"user": "uKey", "device": "phone"},
			wantMethod: http.MethodPost, wantPath: "users/validate.json", wantWithUser: false,
			wantParams: url.Values{"user": {"uKey"}, "device": {"phone"}},
		},
		{
			name:       "get_receipt",
			verb:       "get_receipt",
			opts:       map[string]any{"receipt": "rcpt1"},
			wantMethod: http.MethodGet, wantPath: "receipts/rcpt1.json",
		},
		{
			name:       "cancel_receipt",
			verb:       "cancel_receipt",
			opts:       map[string]any{"receipt": "rcpt1"},
			wantMethod: http.MethodPost, wantPath: "receipts/rcpt1/cancel.json",
		},
		{
			name:       "sounds",
			verb:       "sounds",
			opts:       map[string]any{},
			wantMethod: http.MethodGet, wantPath: "sounds.json",
		},
		{
			name:       "glances",
			verb:       "glances",
			opts:       map[string]any{"title": "t", "text": "x", "subtext": "s", "count": 3, "percent": 50, "device": "watch"},
			wantMethod: http.MethodPost, wantPath: "glances.json", wantWithUser: true,
			wantParams: url.Values{"title": {"t"}, "text": {"x"}, "subtext": {"s"}, "count": {"3"}, "percent": {"50"}, "device": {"watch"}},
		},
		{
			name:       "api escape hatch GET",
			verb:       "api",
			opts:       map[string]any{"method": "get", "path": "/sounds.json", "params": map[string]any{"token": "tok"}},
			wantMethod: http.MethodGet, wantPath: "sounds.json",
			wantParams: url.Values{"token": {"tok"}},
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
			if got.withUser != tc.wantWithUser {
				t.Fatalf("verbCall(%s) withUser = %v, want %v", tc.verb, got.withUser, tc.wantWithUser)
			}
			if tc.wantParams != nil && !reflect.DeepEqual(got.params, tc.wantParams) {
				t.Fatalf("verbCall(%s) params = %#v, want %#v", tc.verb, got.params, tc.wantParams)
			}
		})
	}
}

// TestVerbCallPriority2RequiresRetryExpire proves the priority-2 validation:
// both retry and expire must be present, individually and together.
func TestVerbCallPriority2RequiresRetryExpire(t *testing.T) {
	cases := []map[string]any{
		{"message": "help", "priority": 2},
		{"message": "help", "priority": 2, "retry": 30},
		{"message": "help", "priority": 2, "expire": 3600},
	}
	for _, opts := range cases {
		if _, err := verbCall("send", opts); err == nil {
			t.Errorf("send(%v): expected priority-2 validation error, got nil", opts)
		}
	}
	// priority other than 2 needs neither.
	if _, err := verbCall("send", map[string]any{"message": "hi", "priority": 1}); err != nil {
		t.Errorf("send priority 1 without retry/expire: unexpected error: %v", err)
	}
}

func TestVerbCallErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"send", map[string]any{}},                               // no message
		{"send", map[string]any{"message": "hi", "priority": 3}}, // priority out of range
		{"validate_user", map[string]any{}},                      // no user
		{"get_receipt", map[string]any{}},                        // no receipt
		{"cancel_receipt", map[string]any{}},                     // no receipt
		{"api", map[string]any{"path": "x"}},                     // no method
		{"api", map[string]any{"method": "GET"}},                 // no path
		{"nope", map[string]any{}},                               // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbCall(tc.verb, tc.opts); err == nil {
			t.Errorf("verbCall(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// --- HTTP verb execution against a fake Pushover server ---

func TestDoSendIncludesTokenAndUser(t *testing.T) {
	var gotPath, gotBody, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":1,"request":"abc123"}`))
	}))
	defer srv.Close()

	res, err := pushoverPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "send",
		Connection: map[string]any{"token": "tok", "user": "usr", "api_base": srv.URL},
		Options:    map[string]any{"message": "hello"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotPath != "/messages.json" {
		t.Fatalf("path: got %q", gotPath)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Fatalf("content-type: %q", gotContentType)
	}
	form, err := url.ParseQuery(gotBody)
	if err != nil {
		t.Fatalf("parsing sent body: %v", err)
	}
	if form.Get("token") != "tok" || form.Get("user") != "usr" || form.Get("message") != "hello" {
		t.Fatalf("sent form missing token/user/message: %#v", form)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["request"] != "abc123" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if res.Outputs["status_code"] != http.StatusOK {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

// TestDoSendPriority2ReceiptInResult proves an emergency-priority send's
// receipt surfaces inside `result` (the response is decoded as-is; no extra
// hoisting is needed since Pushover already returns it there).
func TestDoSendPriority2ReceiptInResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":1,"request":"abc123","receipt":"r-xyz"}`))
	}))
	defer srv.Close()

	res, err := pushoverPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "send",
		Connection: map[string]any{"token": "tok", "user": "usr", "api_base": srv.URL},
		Options:    map[string]any{"message": "help", "priority": 2, "retry": 30, "expire": 3600},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["receipt"] != "r-xyz" {
		t.Fatalf("result missing receipt: %#v", res.Outputs["result"])
	}
}

func TestDoGetReceiptUsesQueryToken(t *testing.T) {
	var gotPath, gotQuery, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":1,"acknowledged":0}`))
	}))
	defer srv.Close()

	res, err := pushoverPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "get_receipt",
		Connection: map[string]any{"token": "tok", "user": "usr", "api_base": srv.URL},
		Options:    map[string]any{"receipt": "r-xyz"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("method: got %q want GET", gotMethod)
	}
	if gotPath != "/receipts/r-xyz.json" {
		t.Fatalf("path: got %q", gotPath)
	}
	q, _ := url.ParseQuery(gotQuery)
	if q.Get("token") != "tok" {
		t.Fatalf("query missing token: %q", gotQuery)
	}
	if q.Get("user") != "" {
		t.Fatalf("get_receipt should not include user: %q", gotQuery)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["acknowledged"] != float64(0) {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestDoCancelReceiptPostsTokenOnlyNoUser(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":1}`))
	}))
	defer srv.Close()

	_, err := pushoverPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "cancel_receipt",
		Connection: map[string]any{"token": "tok", "user": "usr", "api_base": srv.URL},
		Options:    map[string]any{"receipt": "r-xyz"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotPath != "/receipts/r-xyz/cancel.json" {
		t.Fatalf("path: got %q", gotPath)
	}
	form, _ := url.ParseQuery(gotBody)
	if form.Get("token") != "tok" {
		t.Fatalf("form missing token: %#v", form)
	}
	if form.Get("user") != "" {
		t.Fatalf("cancel_receipt should not include user: %#v", form)
	}
}

func TestDoValidateUserUsesVerbUserNotConnectionUser(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":1,"devices":["phone"]}`))
	}))
	defer srv.Close()

	_, err := pushoverPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "validate_user",
		Connection: map[string]any{"token": "tok", "user": "connection-user", "api_base": srv.URL},
		Options:    map[string]any{"user": "some-other-user"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	form, _ := url.ParseQuery(gotBody)
	if form.Get("user") != "some-other-user" {
		t.Fatalf("expected the verb's own user, got %#v", form)
	}
	if form.Get("token") != "tok" {
		t.Fatalf("form missing token: %#v", form)
	}
}

func TestDoNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":0,"errors":["user identifier is invalid"]}`))
	}))
	defer srv.Close()

	_, err := pushoverPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "send",
		Connection: map[string]any{"token": "tok", "user": "usr", "api_base": srv.URL},
		Options:    map[string]any{"message": "hi"},
	})
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("expected CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "user identifier is invalid") {
		t.Fatalf("error should surface the response body: %v", pe.Message)
	}
}

func TestInvokeMissingConnection(t *testing.T) {
	_, err := pushoverPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "send",
		Connection: map[string]any{"user": "usr"},
		Options:    map[string]any{"message": "hi"},
	})
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing token, got %v", err)
	}

	_, err = pushoverPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "send",
		Connection: map[string]any{"token": "tok"},
		Options:    map[string]any{"message": "hi"},
	})
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing user, got %v", err)
	}
}

// --- Describe ---

func TestDescribe(t *testing.T) {
	d := pushoverPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "pushover" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Egress, "api.pushover.net:443") {
		t.Fatalf("capabilities.egress: %#v", d.Capabilities.Egress)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("pushover spawns nothing: %#v", d.Capabilities)
	}
	if len(d.Events) != 0 {
		t.Fatalf("pushover is verb-only, no events: %#v", d.Events)
	}
	wantVerbs := []string{"send", "validate_user", "get_receipt", "cancel_receipt", "sounds", "glances", "api"}
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
