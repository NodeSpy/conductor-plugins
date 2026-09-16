package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestBuildRequest pins the exact mode + query params each verb builds — the
// whole contract with the SABnzbd API, proven without spawning anything.
func TestBuildRequest(t *testing.T) {
	cases := []struct {
		name      string
		verb      string
		opts      map[string]any
		wantMode  string
		wantQuery url.Values
		wantHoist []string
	}{
		{
			name:      "queue",
			verb:      "queue",
			opts:      map[string]any{"start": 10, "limit": 5},
			wantMode:  "queue",
			wantQuery: url.Values{"start": {"10"}, "limit": {"5"}},
			wantHoist: []string{"queue", "slots"},
		},
		{
			name:      "queue no options",
			verb:      "queue",
			opts:      map[string]any{},
			wantMode:  "queue",
			wantQuery: url.Values{},
			wantHoist: []string{"queue", "slots"},
		},
		{
			name:      "history",
			verb:      "history",
			opts:      map[string]any{"start": 0, "limit": 20, "category": "movies", "failed_only": true},
			wantMode:  "history",
			wantQuery: url.Values{"start": {"0"}, "limit": {"20"}, "category": {"movies"}, "failed_only": {"1"}},
			wantHoist: []string{"history", "slots"},
		},
		{
			name:      "add_url full",
			verb:      "add_url",
			opts:      map[string]any{"url": "http://example.com/x.nzb", "name": "my job", "category": "movies", "priority": "1", "pp": "3"},
			wantMode:  "addurl",
			wantQuery: url.Values{"name": {"http://example.com/x.nzb"}, "nzbname": {"my job"}, "cat": {"movies"}, "priority": {"1"}, "pp": {"3"}},
		},
		{
			name:      "add_url minimal",
			verb:      "add_url",
			opts:      map[string]any{"url": "http://example.com/x.nzb"},
			wantMode:  "addurl",
			wantQuery: url.Values{"name": {"http://example.com/x.nzb"}},
		},
		{
			name:      "pause",
			verb:      "pause",
			opts:      map[string]any{},
			wantMode:  "pause",
			wantQuery: url.Values{},
		},
		{
			name:      "resume",
			verb:      "resume",
			opts:      map[string]any{},
			wantMode:  "resume",
			wantQuery: url.Values{},
		},
		{
			name:      "pause_job",
			verb:      "pause_job",
			opts:      map[string]any{"value": "SABnzbd_nzo_abc"},
			wantMode:  "queue",
			wantQuery: url.Values{"name": {"pause"}, "value": {"SABnzbd_nzo_abc"}},
		},
		{
			name:      "resume_job",
			verb:      "resume_job",
			opts:      map[string]any{"value": "SABnzbd_nzo_abc"},
			wantMode:  "queue",
			wantQuery: url.Values{"name": {"resume"}, "value": {"SABnzbd_nzo_abc"}},
		},
		{
			name:      "delete_job with files",
			verb:      "delete_job",
			opts:      map[string]any{"value": "all", "del_files": true},
			wantMode:  "queue",
			wantQuery: url.Values{"name": {"delete"}, "value": {"all"}, "del_files": {"1"}},
		},
		{
			name:      "delete_job without files",
			verb:      "delete_job",
			opts:      map[string]any{"value": "SABnzbd_nzo_abc"},
			wantMode:  "queue",
			wantQuery: url.Values{"name": {"delete"}, "value": {"SABnzbd_nzo_abc"}},
		},
		{
			name:      "set_speedlimit",
			verb:      "set_speedlimit",
			opts:      map[string]any{"value": "50"},
			wantMode:  "config",
			wantQuery: url.Values{"name": {"speedlimit"}, "value": {"50"}},
		},
		{
			name:      "status",
			verb:      "status",
			opts:      map[string]any{},
			wantMode:  "fullstatus",
			wantQuery: url.Values{},
		},
		{
			name:      "version",
			verb:      "version",
			opts:      map[string]any{},
			wantMode:  "version",
			wantQuery: url.Values{},
		},
		{
			name:      "categories",
			verb:      "categories",
			opts:      map[string]any{},
			wantMode:  "get_cats",
			wantQuery: url.Values{},
			wantHoist: []string{"categories"},
		},
		{
			name:      "api generic",
			verb:      "api",
			opts:      map[string]any{"mode": "get_config", "params": map[string]any{"section": "misc"}},
			wantMode:  "get_config",
			wantQuery: url.Values{"section": {"misc"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rb, err := buildRequest(tc.verb, tc.opts)
			if err != nil {
				t.Fatalf("buildRequest(%s): unexpected error: %v", tc.verb, err)
			}
			if rb.mode != tc.wantMode {
				t.Errorf("mode: got %q want %q", rb.mode, tc.wantMode)
			}
			if !reflect.DeepEqual(rb.query, tc.wantQuery) {
				t.Errorf("query:\n got: %#v\nwant: %#v", rb.query, tc.wantQuery)
			}
			if !reflect.DeepEqual(rb.hoist, tc.wantHoist) {
				t.Errorf("hoist: got %#v want %#v", rb.hoist, tc.wantHoist)
			}
		})
	}
}

// TestBuildRequestErrors covers required-field validation and unknown verbs.
func TestBuildRequestErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"add_url", map[string]any{}},        // no url
		{"pause_job", map[string]any{}},      // no value
		{"resume_job", map[string]any{}},     // no value
		{"delete_job", map[string]any{}},     // no value
		{"set_speedlimit", map[string]any{}}, // no value
		{"api", map[string]any{}},            // no mode
		{"nope", map[string]any{}},           // unknown verb
	}
	for _, tc := range cases {
		if _, err := buildRequest(tc.verb, tc.opts); err == nil {
			t.Errorf("buildRequest(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// newFakeSAB starts an httptest.Server standing in for SABnzbd, asserting the
// GET /api request carries apikey/output=json/mode, and returning the given
// JSON body.
func newFakeSAB(t *testing.T, wantMode string, checkQuery func(t *testing.T, q url.Values), body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api" {
			t.Errorf("path: got %q want /api", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("apikey") != "test-key" {
			t.Errorf("apikey: got %q want test-key", q.Get("apikey"))
		}
		if q.Get("output") != "json" {
			t.Errorf("output: got %q want json", q.Get("output"))
		}
		if q.Get("mode") != wantMode {
			t.Errorf("mode: got %q want %q", q.Get("mode"), wantMode)
		}
		if checkQuery != nil {
			checkQuery(t, q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestInvokeQueueHoistsSlots(t *testing.T) {
	srv := newFakeSAB(t, "queue", func(t *testing.T, q url.Values) {
		if q.Get("start") != "0" || q.Get("limit") != "10" {
			t.Errorf("query: %#v", q)
		}
	}, `{"queue":{"status":"Downloading","slots":[{"nzo_id":"a"},{"nzo_id":"b"}]}}`)
	defer srv.Close()

	p := sabnzbdPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "queue",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options:    map[string]any{"start": 0, "limit": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Fatalf("status_code: %#v", res.Outputs["status_code"])
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
	if res.Outputs["result"] == nil {
		t.Fatal("result should carry the whole decoded object")
	}
}

func TestInvokeHistoryHoistsSlots(t *testing.T) {
	srv := newFakeSAB(t, "history", nil, `{"history":{"slots":[{"nzo_id":"c"}],"noofslots":1}}`)
	defer srv.Close()

	p := sabnzbdPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "history",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeCategoriesHoistsTopLevelList(t *testing.T) {
	srv := newFakeSAB(t, "get_cats", nil, `{"categories":["*","movies","tv"]}`)
	defer srv.Close()

	p := sabnzbdPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "categories",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 3 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestInvokeAddURLParams(t *testing.T) {
	srv := newFakeSAB(t, "addurl", func(t *testing.T, q url.Values) {
		if q.Get("name") != "http://example.com/x.nzb" {
			t.Errorf("name: %q", q.Get("name"))
		}
		if q.Get("nzbname") != "my job" {
			t.Errorf("nzbname: %q", q.Get("nzbname"))
		}
		if q.Get("cat") != "movies" {
			t.Errorf("cat: %q", q.Get("cat"))
		}
		if q.Get("priority") != "1" {
			t.Errorf("priority: %q", q.Get("priority"))
		}
	}, `{"status":true,"nzo_ids":["SABnzbd_nzo_new"]}`)
	defer srv.Close()

	p := sabnzbdPlugin{}
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "add_url",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "test-key"},
		Options: map[string]any{
			"url": "http://example.com/x.nzb", "name": "my job",
			"category": "movies", "priority": "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["items"]; ok {
		t.Fatalf("add_url should not produce items: %#v", res.Outputs)
	}
	if res.Outputs["result"] == nil {
		t.Fatal("result should be set")
	}
}

func TestInvokeNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"API Key Incorrect"}`))
	}))
	defer srv.Close()

	p := sabnzbdPlugin{}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "version",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "bad-key"},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "401") || !strings.Contains(pe.Message, "API Key Incorrect") {
		t.Fatalf("error message should carry status + body: %q", pe.Message)
	}
}

func TestInvokeMissingConnection(t *testing.T) {
	p := sabnzbdPlugin{}
	cases := []map[string]any{
		{},
		{"base_url": "http://sab:8080"},
		{"api_key": "k"},
	}
	for _, conn := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: "version", Connection: conn})
		if err == nil {
			t.Fatalf("connection %#v: expected error", conn)
		}
		var pe *plugin.Error
		if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
			t.Fatalf("connection %#v: want CodeInvalidParams, got %v", conn, err)
		}
	}
}

// TestDescribe asserts the declared surface: kind, type, capabilities, and
// every verb.
func TestDescribe(t *testing.T) {
	d := sabnzbdPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "sabnzbd" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("capabilities: verb-only connector must not spawn/command: %#v", d.Capabilities)
	}
	if d.Capabilities.Egress == nil || len(d.Capabilities.Egress) != 0 {
		t.Fatalf("capabilities.egress: want declared-empty ([]string{}), got %#v", d.Capabilities.Egress)
	}
	want := []string{"queue", "history", "add_url", "pause", "resume", "pause_job",
		"resume_job", "delete_job", "set_speedlimit", "status", "version", "categories", "api"}
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
	if len(d.Events) != 0 {
		t.Fatalf("sabnzbd is verb-only: no events expected, got %#v", d.Events)
	}
}

// asPluginError unwraps a *plugin.Error without importing errors just for the
// test (the SDK returns the concrete type directly).
func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}
