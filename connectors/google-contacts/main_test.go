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

func TestConnectionsHoistsItemsAndSendsPersonFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/people/me/connections" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("personFields"); got != defaultPersonFields {
			t.Errorf("personFields: got %q want %q", got, defaultPersonFields)
		}
		writeJSON(w, 200, map[string]any{"connections": []any{
			map[string]any{"resourceName": "people/c1", "names": []any{map[string]any{"displayName": "Ada"}}},
		}})
	}))
	defer srv.Close()

	p := newGoogleContactsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "connections", Connection: testConn(srv)})
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
	if items[0].(map[string]any)["resourceName"] != "people/c1" {
		t.Errorf("items[0]: %#v", items[0])
	}
}

func TestConnectionsCustomPersonFieldsAndPaging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		q := r.URL.Query()
		if q.Get("personFields") != "names,addresses" || q.Get("pageSize") != "50" ||
			q.Get("pageToken") != "tok123" || q.Get("sortOrder") != "FIRST_NAME_ASCENDING" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{"connections": []any{}})
	}))
	defer srv.Close()

	p := newGoogleContactsPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "connections", Connection: testConn(srv),
		Options: map[string]any{
			"personFields": "names,addresses", "pageSize": 50,
			"pageToken": "tok123", "sortOrder": "FIRST_NAME_ASCENDING",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContactGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/people/c1234567890" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("personFields"); got != defaultPersonFields {
			t.Errorf("personFields: got %q want %q", got, defaultPersonFields)
		}
		writeJSON(w, 200, map[string]any{"resourceName": "people/c1234567890", "names": []any{map[string]any{"displayName": "Ada"}}})
	}))
	defer srv.Close()

	p := newGoogleContactsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "contact_get", Connection: testConn(srv),
		Options: map[string]any{"resource_name": "people/c1234567890"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["resourceName"] != "people/c1234567890" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestContactCreateConvenienceFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/people:createContact" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"resourceName": "people/c-new"})
	}))
	defer srv.Close()

	p := newGoogleContactsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "contact_create", Connection: testConn(srv),
		Options: map[string]any{
			"given_name":  "Ada",
			"family_name": "Lovelace",
			"email":       "ada@example.com",
			"phone":       "+15551234567",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	names, ok := gotBody["names"].([]any)
	if !ok || len(names) != 1 {
		t.Fatalf("names: %#v", gotBody["names"])
	}
	name := names[0].(map[string]any)
	if name["givenName"] != "Ada" || name["familyName"] != "Lovelace" {
		t.Fatalf("name: %#v", name)
	}
	emails, ok := gotBody["emailAddresses"].([]any)
	if !ok || len(emails) != 1 || emails[0].(map[string]any)["value"] != "ada@example.com" {
		t.Fatalf("emailAddresses: %#v", gotBody["emailAddresses"])
	}
	phones, ok := gotBody["phoneNumbers"].([]any)
	if !ok || len(phones) != 1 || phones[0].(map[string]any)["value"] != "+15551234567" {
		t.Fatalf("phoneNumbers: %#v", gotBody["phoneNumbers"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["resourceName"] != "people/c-new" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestContactCreatePersonMapOverridesConvenience(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"resourceName": "people/c-raw"})
	}))
	defer srv.Close()

	p := newGoogleContactsPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "contact_create", Connection: testConn(srv),
		Options: map[string]any{
			"given_name": "should be ignored",
			"person":     map[string]any{"names": []any{map[string]any{"givenName": "Raw"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	names, ok := gotBody["names"].([]any)
	if !ok || len(names) != 1 || names[0].(map[string]any)["givenName"] != "Raw" {
		t.Fatalf("body should come from person map, got %#v", gotBody)
	}
}

func TestContactUpdate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPatch || r.URL.Path != "/people/c1:updateContact" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("updatePersonFields"); got != defaultPersonFields {
			t.Errorf("updatePersonFields: got %q want %q", got, defaultPersonFields)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"resourceName": "people/c1", "names": gotBody["names"]})
	}))
	defer srv.Close()

	p := newGoogleContactsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "contact_update", Connection: testConn(srv),
		Options: map[string]any{
			"resource_name": "people/c1",
			"person":        map[string]any{"names": []any{map[string]any{"givenName": "Updated"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	names, ok := gotBody["names"].([]any)
	if !ok || names[0].(map[string]any)["givenName"] != "Updated" {
		t.Fatalf("body: %#v", gotBody)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["resourceName"] != "people/c1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestContactDelete(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	p := newGoogleContactsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "contact_delete", Connection: testConn(srv),
		Options: map[string]any{"resource_name": "people/c1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/people/c1:deleteContact" {
		t.Fatalf("request: %s %s", gotMethod, gotPath)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestSearchSendsQueryAndReadMask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/people:searchContacts" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("query") != "ada" || q.Get("readMask") != defaultPersonFields {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{"results": []any{
			map[string]any{"person": map[string]any{"resourceName": "people/c1"}},
		}})
	}))
	defer srv.Close()

	p := newGoogleContactsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "search", Connection: testConn(srv),
		Options: map[string]any{"query": "ada"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestOtherContacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/otherContacts" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("readMask"); got != defaultPersonFields {
			t.Errorf("readMask: got %q want %q", got, defaultPersonFields)
		}
		writeJSON(w, 200, map[string]any{"otherContacts": []any{map[string]any{"resourceName": "otherContacts/c1"}}})
	}))
	defer srv.Close()

	p := newGoogleContactsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "other_contacts", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["resourceName"] != "otherContacts/c1" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/contactGroups" && r.URL.Query().Get("pageSize") == "5":
			writeJSON(w, 200, []any{map[string]any{"resourceName": "contactGroups/g1"}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newGoogleContactsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/contactGroups", "query": map[string]any{"pageSize": "5"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["resourceName"] != "contactGroups/g1" {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAPIEscapeHatchObjectResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/people:createContact" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"resourceName": "people/c-api"})
	}))
	defer srv.Close()

	p := newGoogleContactsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "POST", "path": "/people:createContact", "body": map[string]any{"names": []any{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["resourceName"] != "people/c-api" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	defer srv.Close()

	p := newGoogleContactsPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "connections", Connection: testConn(srv)})
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
	p := newGoogleContactsPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "connections",
		Connection: map[string]any{}, // no access_token injected
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing access token, got %v", err)
	}
	if !containsAll(pe.Message, "access token", "conductor connector auth google-contacts") {
		t.Errorf("message should point at the auth: block / login flow, got %q", pe.Message)
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newGoogleContactsPlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"contact_get", map[string]any{}},
		{"contact_update", map[string]any{}},
		{"contact_update", map[string]any{"resource_name": "people/c1"}}, // missing person map
		{"contact_delete", map[string]any{}},
		{"search", map[string]any{}},
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
	p := newGoogleContactsPlugin()
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
	p := newGoogleContactsPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "google-contacts" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["api_base"].Type != "string" || d.Connection["api_base"].Required {
		t.Errorf("connection.api_base: %#v", d.Connection["api_base"])
	}

	want := []string{
		"connections", "contact_get", "contact_create", "contact_update",
		"contact_delete", "search", "other_contacts", "api",
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

	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "people.googleapis.com:443" {
		t.Errorf("egress: got %v want [people.googleapis.com:443]", d.Capabilities.Egress)
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
	foundContactsScope := false
	for _, s := range d.Auth.Scopes {
		if s == "https://www.googleapis.com/auth/contacts" {
			foundContactsScope = true
		}
	}
	if !foundContactsScope {
		t.Errorf("Auth.Scopes missing contacts scope: %v", d.Auth.Scopes)
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
