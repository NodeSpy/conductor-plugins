package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// testConn builds a connection map with the managed-OAuth2 token already
// injected (as the daemon would do), pointed at srv.
func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{
		plugin.AccessTokenKey: "test-token",
		"api_base":            srv.URL,
		"calendar_id":         "primary",
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

func TestCalendarsVerb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/users/me/calendarList" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"items": []any{
			map[string]any{"id": "primary", "summary": "Daniel"},
		}})
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "calendars", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	if items[0].(map[string]any)["id"] != "primary" {
		t.Errorf("items[0]: %#v", items[0])
	}
}

func TestEventsHoistsItemsAndSendsQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/calendars/primary/events" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("timeMin") != "2026-01-01T00:00:00Z" || q.Get("timeMax") != "2026-02-01T00:00:00Z" ||
			q.Get("q") != "standup" || q.Get("maxResults") != "10" || q.Get("singleEvents") != "true" ||
			q.Get("orderBy") != "startTime" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{"items": []any{map[string]any{"id": "ev1"}}})
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "events", Connection: testConn(srv),
		Options: map[string]any{
			"timeMin": "2026-01-01T00:00:00Z", "timeMax": "2026-02-01T00:00:00Z",
			"q": "standup", "maxResults": 10, "singleEvents": true, "orderBy": "startTime",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "ev1" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestEventsCalendarIDOptionOverridesConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/calendars/work@example.com/events" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"items": []any{}})
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "events", Connection: testConn(srv),
		Options: map[string]any{"calendar_id": "work@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEventGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/calendars/primary/events/ev1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": "ev1", "summary": "Standup"})
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "event_get", Connection: testConn(srv),
		Options: map[string]any{"event_id": "ev1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["summary"] != "Standup" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestEventCreateConvenienceFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/calendars/primary/events" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"id": "ev-new", "summary": gotBody["summary"]})
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "event_create", Connection: testConn(srv),
		Options: map[string]any{
			"summary":     "Lunch",
			"description": "with the team",
			"start":       "2026-03-01T12:00:00Z",
			"end":         "2026-03-01T13:00:00Z",
			"attendees":   []any{"a@example.com", "b@example.com"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["summary"] != "Lunch" || gotBody["description"] != "with the team" {
		t.Fatalf("body: %#v", gotBody)
	}
	start, ok := gotBody["start"].(map[string]any)
	if !ok || start["dateTime"] != "2026-03-01T12:00:00Z" {
		t.Fatalf("start: %#v", gotBody["start"])
	}
	attendees, ok := gotBody["attendees"].([]any)
	if !ok || len(attendees) != 2 || attendees[0].(map[string]any)["email"] != "a@example.com" {
		t.Fatalf("attendees: %#v", gotBody["attendees"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "ev-new" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestEventCreateEventMapOverridesConvenience(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"id": "ev-raw"})
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "event_create", Connection: testConn(srv),
		Options: map[string]any{
			"summary": "should be ignored",
			"event":   map[string]any{"summary": "Raw event", "recurrence": []any{"RRULE:FREQ=DAILY"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["summary"] != "Raw event" {
		t.Fatalf("body should come from event map, got %#v", gotBody)
	}
	if _, ok := gotBody["recurrence"]; !ok {
		t.Fatalf("body missing recurrence: %#v", gotBody)
	}
}

func TestEventUpdate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPatch || r.URL.Path != "/calendars/primary/events/ev1" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"id": "ev1", "summary": gotBody["summary"]})
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "event_update", Connection: testConn(srv),
		Options: map[string]any{"event_id": "ev1", "event": map[string]any{"summary": "Updated title"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["summary"] != "Updated title" {
		t.Fatalf("body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["summary"] != "Updated title" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestEventDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "event_delete", Connection: testConn(srv),
		Options: map[string]any{"event_id": "ev1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/calendars/primary/events/ev1" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if res.Outputs["status_code"] != 204 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestQuickAdd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/calendars/primary/events/quickAdd" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("text") != "Lunch tomorrow 1pm" {
			t.Errorf("query: got %q", r.URL.RawQuery)
		}
		writeJSON(w, 200, map[string]any{"id": "ev-quick"})
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "quick_add", Connection: testConn(srv),
		Options: map[string]any{"text": "Lunch tomorrow 1pm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "ev-quick" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestFreebusy(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/freeBusy" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"calendars": map[string]any{"primary": map[string]any{"busy": []any{}}}})
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "freebusy", Connection: testConn(srv),
		Options: map[string]any{"timeMin": "2026-01-01T00:00:00Z", "timeMax": "2026-01-02T00:00:00Z"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["timeMin"] != "2026-01-01T00:00:00Z" || gotBody["timeMax"] != "2026-01-02T00:00:00Z" {
		t.Fatalf("body: %#v", gotBody)
	}
	items, ok := gotBody["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "primary" {
		t.Fatalf("body items (default to connection calendar_id): %#v", gotBody["items"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := result["calendars"]; !ok {
		t.Fatalf("result missing calendars: %#v", result)
	}
}

func TestFreebusyExplicitItems(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		writeJSON(w, 200, map[string]any{"calendars": map[string]any{}})
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "freebusy", Connection: testConn(srv),
		Options: map[string]any{
			"timeMin": "2026-01-01T00:00:00Z", "timeMax": "2026-01-02T00:00:00Z",
			"items": []any{"a@example.com", "b@example.com"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := gotBody["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", gotBody["items"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/me/settings" && r.URL.Query().Get("maxResults") == "5":
			writeJSON(w, 200, []any{map[string]any{"id": "s1"}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/users/me/settings", "query": map[string]any{"maxResults": "5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != "s1" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAPIEscapeHatchObjectResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/calendars/primary/events" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"id": "ev-api"})
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "POST", "path": "/calendars/primary/events", "body": map[string]any{"summary": "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "ev-api" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	defer srv.Close()

	p := newGoogleCalendarPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "calendars", Connection: testConn(srv)})
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
	p := newGoogleCalendarPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "calendars",
		Connection: map[string]any{"calendar_id": "primary"}, // no access_token injected
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing access token, got %v", err)
	}
	if !containsAll(pe.Message, "access token", "conductor connector auth google-calendar") {
		t.Errorf("message should point at the auth: block / login flow, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newGoogleCalendarPlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t", "calendar_id": "primary"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"event_get", map[string]any{}},
		{"event_update", map[string]any{}},
		{"event_update", map[string]any{"event_id": "ev1"}}, // missing event map
		{"event_delete", map[string]any{}},
		{"quick_add", map[string]any{}},
		{"freebusy", map[string]any{}},
		{"freebusy", map[string]any{"timeMin": "2026-01-01T00:00:00Z"}}, // missing timeMax
		{"api", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("%s %v: expected error for missing required options", tc.verb, tc.opts)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s %v: expected CodeInvalidParams, got %v", tc.verb, tc.opts, err)
		}
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newGoogleCalendarPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{plugin.AccessTokenKey: "t"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for unknown verb, got %v", err)
	}
}

func TestDescribe(t *testing.T) {
	p := newGoogleCalendarPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "google-calendar" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["calendar_id"].Type != "string" || d.Connection["calendar_id"].Required {
		t.Errorf("connection.calendar_id: %#v", d.Connection["calendar_id"])
	}
	if d.Connection["api_base"].Type != "string" || d.Connection["api_base"].Required {
		t.Errorf("connection.api_base: %#v", d.Connection["api_base"])
	}

	want := []string{
		"calendars", "events", "event_get", "event_create", "event_update",
		"event_delete", "quick_add", "freebusy", "api",
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

	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "www.googleapis.com:443" {
		t.Errorf("egress: got %v want [www.googleapis.com:443]", d.Capabilities.Egress)
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
	foundCalendarScope := false
	for _, s := range d.Auth.Scopes {
		if s == "https://www.googleapis.com/auth/calendar" {
			foundCalendarScope = true
		}
	}
	if !foundCalendarScope {
		t.Errorf("Auth.Scopes missing calendar scope: %v", d.Auth.Scopes)
	}
	if d.Auth.AuthParams["access_type"] != "offline" {
		t.Errorf("Auth.AuthParams[access_type]: got %q want %q", d.Auth.AuthParams["access_type"], "offline")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (sub == "" || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
