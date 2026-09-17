package main

import (
	"encoding/json"
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

func TestGetVerb(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/sheet-1" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"spreadsheetId": "sheet-1", "properties": map[string]any{"title": "My Sheet"}})
	}))
	defer srv.Close()

	p := newSheetsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "get", Connection: testConn(srv),
		Options: map[string]any{"spreadsheet_id": "sheet-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["spreadsheetId"] != "sheet-1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestValuesGetReturnsResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/sheet-1/values/Sheet1!A1:C10" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{
			"range":  "Sheet1!A1:C10",
			"values": []any{[]any{"a", "b", "c"}},
		})
	}))
	defer srv.Close()

	p := newSheetsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "values_get", Connection: testConn(srv),
		Options: map[string]any{"spreadsheet_id": "sheet-1", "range": "Sheet1!A1:C10"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := result["values"]; !ok {
		t.Fatalf("result missing values: %#v", result)
	}
}

func TestValuesUpdateSendsValuesAndValueInputOption(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPut {
			t.Errorf("method: got %q want PUT", r.Method)
		}
		if r.URL.Path != "/sheet-1/values/Sheet1!A1:B2" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("valueInputOption"); got != "USER_ENTERED" {
			t.Errorf("valueInputOption: got %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		values, ok := body["values"].([]any)
		if !ok || len(values) != 1 {
			t.Fatalf("body values: %#v", body["values"])
		}
		writeJSON(w, 200, map[string]any{"spreadsheetId": "sheet-1", "updatedCells": 2})
	}))
	defer srv.Close()

	p := newSheetsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "values_update", Connection: testConn(srv),
		Options: map[string]any{
			"spreadsheet_id": "sheet-1", "range": "Sheet1!A1:B2",
			"values": []any{[]any{"x", "y"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["updatedCells"] != float64(2) {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestValuesAppendHitsAppendPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q want POST", r.Method)
		}
		if r.URL.Path != "/sheet-1/values/Sheet1!A1:B2:append" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("valueInputOption") != "USER_ENTERED" || q.Get("insertDataOption") != "INSERT_ROWS" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{"spreadsheetId": "sheet-1"})
	}))
	defer srv.Close()

	p := newSheetsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "values_append", Connection: testConn(srv),
		Options: map[string]any{
			"spreadsheet_id": "sheet-1", "range": "Sheet1!A1:B2",
			"values": []any{[]any{"x", "y"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestValuesClear(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q want POST", r.Method)
		}
		if r.URL.Path != "/sheet-1/values/Sheet1!A1:B2:clear" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"spreadsheetId": "sheet-1", "clearedRange": "Sheet1!A1:B2"})
	}))
	defer srv.Close()

	p := newSheetsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "values_clear", Connection: testConn(srv),
		Options: map[string]any{"spreadsheet_id": "sheet-1", "range": "Sheet1!A1:B2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["clearedRange"] != "Sheet1!A1:B2" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestValuesBatchGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.URL.Path != "/sheet-1/values:batchGet" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		ranges := r.URL.Query()["ranges"]
		if len(ranges) != 2 || ranges[0] != "Sheet1!A:A" || ranges[1] != "Sheet2!A:A" {
			t.Errorf("ranges: got %v", ranges)
		}
		writeJSON(w, 200, map[string]any{"valueRanges": []any{}})
	}))
	defer srv.Close()

	p := newSheetsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "values_batch_get", Connection: testConn(srv),
		Options: map[string]any{"spreadsheet_id": "sheet-1", "ranges": []any{"Sheet1!A:A", "Sheet2!A:A"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestBatchUpdate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q want POST", r.Method)
		}
		if r.URL.Path != "/sheet-1:batchUpdate" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if _, ok := body["requests"].([]any); !ok {
			t.Fatalf("body requests: %#v", body["requests"])
		}
		writeJSON(w, 200, map[string]any{"spreadsheetId": "sheet-1", "replies": []any{}})
	}))
	defer srv.Close()

	p := newSheetsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "batch_update", Connection: testConn(srv),
		Options: map[string]any{
			"spreadsheet_id": "sheet-1",
			"requests":       []any{map[string]any{"addSheet": map[string]any{}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q want POST", r.Method)
		}
		if r.URL.Path != "/" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		props, ok := body["properties"].(map[string]any)
		if !ok || props["title"] != "New Sheet" {
			t.Fatalf("body properties: %#v", body["properties"])
		}
		writeJSON(w, 200, map[string]any{"spreadsheetId": "new-1", "properties": map[string]any{"title": "New Sheet"}})
	}))
	defer srv.Close()

	p := newSheetsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "create", Connection: testConn(srv),
		Options: map[string]any{"title": "New Sheet"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["spreadsheetId"] != "new-1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkBearer(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sheet-1" && r.URL.Query().Get("includeGridData") == "true":
			writeJSON(w, 200, map[string]any{"spreadsheetId": "sheet-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/sheet-1:batchUpdate":
			writeJSON(w, 200, map[string]any{"spreadsheetId": "sheet-1", "replies": []any{}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newSheetsPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/sheet-1", "query": map[string]any{"includeGridData": "true"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["spreadsheetId"] != "sheet-1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "POST", "path": "sheet-1:batchUpdate", "body": map[string]any{"requests": []any{}}},
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
		if r.URL.Path != "/sheet-1/values/Sheet1!A:A" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, []any{"a", "b"})
	}))
	defer srv.Close()

	p := newSheetsPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/sheet-1/values/Sheet1!A:A"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestMissingSpreadsheetIDIsInvalidParams(t *testing.T) {
	p := newSheetsPlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"get", map[string]any{}},
		{"values_get", map[string]any{}},
		{"values_update", map[string]any{"range": "A1"}},
		{"values_append", map[string]any{"range": "A1"}},
		{"values_clear", map[string]any{}},
		{"values_batch_get", map[string]any{}},
		{"batch_update", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("%s: expected error for missing spreadsheet_id", tc.verb)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s: expected CodeInvalidParams, got %v", tc.verb, err)
		}
	}
}

func TestMissingRangeIsInvalidParams(t *testing.T) {
	p := newSheetsPlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t"}

	cases := []string{"values_get", "values_update", "values_append", "values_clear"}
	for _, verb := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{
			Verb: verb, Connection: conn,
			Options: map[string]any{"spreadsheet_id": "sheet-1"},
		})
		if err == nil {
			t.Errorf("%s: expected error for missing range", verb)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s: expected CodeInvalidParams, got %v", verb, err)
		}
	}
}

func TestMissingValuesIsInvalidParams(t *testing.T) {
	p := newSheetsPlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t"}

	cases := []string{"values_update", "values_append"}
	for _, verb := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{
			Verb: verb, Connection: conn,
			Options: map[string]any{"spreadsheet_id": "sheet-1", "range": "A1"},
		})
		if err == nil {
			t.Errorf("%s: expected error for missing values", verb)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s: expected CodeInvalidParams, got %v", verb, err)
		}
	}
}

func TestMissingRequestsIsInvalidParams(t *testing.T) {
	p := newSheetsPlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t"}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "batch_update", Connection: conn,
		Options: map[string]any{"spreadsheet_id": "sheet-1"},
	})
	if err == nil {
		t.Fatal("expected error for missing requests")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

func TestMissingPathIsInvalidParams(t *testing.T) {
	p := newSheetsPlugin()
	conn := map[string]any{plugin.AccessTokenKey: "t"}
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "api", Connection: conn, Options: map[string]any{}})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

func TestMissingAccessTokenIsInvalidParams(t *testing.T) {
	p := newSheetsPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "get",
		Connection: map[string]any{"spreadsheet_id": "sheet-1"}, // no access_token injected
		Options:    map[string]any{"spreadsheet_id": "sheet-1"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams for missing access token, got %v", err)
	}
	if !containsAll(pe.Message, "access token", "conductor connector auth google-sheets") {
		t.Errorf("message should point at the auth: block / login flow, got %q", pe.Message)
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid_token"}}`))
	}))
	defer srv.Close()

	p := newSheetsPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "get", Connection: testConn(srv),
		Options: map[string]any{"spreadsheet_id": "sheet-1"},
	})
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

func TestUnknownVerb(t *testing.T) {
	p := newSheetsPlugin()
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
	p := newSheetsPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "google-sheets" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only, no source), got %v", d.Events)
	}
	if d.Connection["api_base"].Type != "string" || d.Connection["api_base"].Required {
		t.Errorf("connection.api_base: %#v", d.Connection["api_base"])
	}

	want := []string{
		"get", "values_get", "values_update", "values_append", "values_clear",
		"values_batch_get", "batch_update", "create", "api",
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

	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "sheets.googleapis.com:443" {
		t.Errorf("egress: got %v want [sheets.googleapis.com:443]", d.Capabilities.Egress)
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
	foundSpreadsheetsScope := false
	for _, s := range d.Auth.Scopes {
		if s == "https://www.googleapis.com/auth/spreadsheets" {
			foundSpreadsheetsScope = true
		}
	}
	if !foundSpreadsheetsScope {
		t.Errorf("Auth.Scopes missing spreadsheets scope: %v", d.Auth.Scopes)
	}
	if d.Auth.AuthParams["access_type"] != "offline" {
		t.Errorf("Auth.AuthParams[access_type]: got %q want %q", d.Auth.AuthParams["access_type"], "offline")
	}
	if d.Auth.AuthParams["prompt"] != "consent" {
		t.Errorf("Auth.AuthParams[prompt]: got %q want %q", d.Auth.AuthParams["prompt"], "consent")
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
