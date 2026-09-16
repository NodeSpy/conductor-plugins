package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// TestDescribe asserts the declared surface: kind, type, capabilities, and
// every verb.
func TestDescribe(t *testing.T) {
	d := notifiarrPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "notifiarr" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if !contains(d.Capabilities.Egress, "notifiarr.com:443") {
		t.Fatalf("egress: %#v", d.Capabilities.Egress)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("expected no commands/spawns, got %#v", d.Capabilities)
	}
	want := []string{"passthrough", "api"}
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
}

// TestVerbCall pins the exact HTTP method/path/body each verb builds,
// including the passthrough ergonomic-option assembly — proven without
// sending anything.
func TestVerbCall(t *testing.T) {
	cases := []struct {
		name       string
		verb       string
		opts       map[string]any
		apiKey     string
		wantMethod string
		wantPath   string
		wantBody   any
	}{
		{
			name: "passthrough minimal",
			verb: "passthrough",
			opts: map[string]any{
				"title":      "Backup finished",
				"message":    "the nightly backup completed",
				"channel_id": "1234567890",
			},
			apiKey:     "key-abc",
			wantMethod: http.MethodPost,
			wantPath:   "notification/passthrough/key-abc",
			wantBody: map[string]any{
				"notification": map[string]any{
					"update": false,
					"name":   "Backup finished",
					"event":  "",
				},
				"discord": map[string]any{
					"color": "",
					"ping": map[string]any{
						"pingUser": nil,
						"pingRole": nil,
					},
					"images": map[string]any{
						"thumbnail": nil,
						"image":     nil,
					},
					"text": map[string]any{
						"title":       "Backup finished",
						"icon":        nil,
						"content":     "",
						"description": "the nightly backup completed",
						"fields":      nil,
						"footer":      nil,
					},
					"ids": map[string]any{
						"channel": "1234567890",
					},
				},
			},
		},
		{
			name: "passthrough full",
			verb: "passthrough",
			opts: map[string]any{
				"title":      "Deploy failed",
				"message":    "svc-api deploy failed",
				"channel_id": "555",
				"color":      "ff0000",
				"event":      "deploy.failed",
				"ping_user":  "111",
				"ping_role":  "222",
				"icon":       "https://example.com/icon.png",
				"image":      "https://example.com/image.png",
				"thumbnail":  "https://example.com/thumb.png",
				"footer":     "conductor",
				"fields": []any{
					map[string]any{"title": "service", "text": "svc-api", "inline": true},
				},
			},
			apiKey:     "key-abc",
			wantMethod: http.MethodPost,
			wantPath:   "notification/passthrough/key-abc",
			wantBody: map[string]any{
				"notification": map[string]any{
					"update": false,
					"name":   "Deploy failed",
					"event":  "deploy.failed",
				},
				"discord": map[string]any{
					"color": "ff0000",
					"ping": map[string]any{
						"pingUser": "111",
						"pingRole": "222",
					},
					"images": map[string]any{
						"thumbnail": "https://example.com/thumb.png",
						"image":     "https://example.com/image.png",
					},
					"text": map[string]any{
						"title":       "Deploy failed",
						"icon":        "https://example.com/icon.png",
						"content":     "",
						"description": "svc-api deploy failed",
						"fields": []any{
							map[string]any{"title": "service", "text": "svc-api", "inline": true},
						},
						"footer": "conductor",
					},
					"ids": map[string]any{
						"channel": "555",
					},
				},
			},
		},
		{
			name:       "api escape hatch",
			verb:       "api",
			opts:       map[string]any{"method": "get", "path": "/user"},
			apiKey:     "key-abc",
			wantMethod: http.MethodGet,
			wantPath:   "user",
			wantBody:   nil,
		},
		{
			name:       "api escape hatch with body",
			verb:       "api",
			opts:       map[string]any{"method": "post", "path": "user/notifications", "body": map[string]any{"x": 1}},
			apiKey:     "key-abc",
			wantMethod: http.MethodPost,
			wantPath:   "user/notifications",
			wantBody:   map[string]any{"x": float64(1)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, err := verbCall(tc.verb, tc.opts, tc.apiKey)
			if err != nil {
				t.Fatalf("verbCall(%s): unexpected error: %v", tc.verb, err)
			}
			if call.method != tc.wantMethod {
				t.Errorf("method: got %q want %q", call.method, tc.wantMethod)
			}
			if call.path != tc.wantPath {
				t.Errorf("path: got %q want %q", call.path, tc.wantPath)
			}
			assertJSONEqual(t, call.body, tc.wantBody)
		})
	}
}

// TestVerbCallErrors covers required-field validation and unknown verbs.
func TestVerbCallErrors(t *testing.T) {
	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"passthrough", map[string]any{}},                             // no title/message/channel_id
		{"passthrough", map[string]any{"title": "t"}},                 // no message/channel_id
		{"passthrough", map[string]any{"title": "t", "message": "m"}}, // no channel_id
		{"api", map[string]any{}},                                     // no method/path
		{"api", map[string]any{"method": "GET"}},                      // no path
		{"nope", map[string]any{}},                                    // unknown verb
	}
	for _, tc := range cases {
		if _, err := verbCall(tc.verb, tc.opts, "key"); err == nil {
			t.Errorf("verbCall(%s, %v): expected error, got nil", tc.verb, tc.opts)
		}
	}
}

// TestParseConn covers connection validation and defaults.
func TestParseConn(t *testing.T) {
	if _, err := parseConn(map[string]any{}); err == nil {
		t.Fatal("expected error for missing api_key")
	}
	c, err := parseConn(map[string]any{"api_key": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if c.base != defaultAPIBase {
		t.Errorf("base default: got %q want %q", c.base, defaultAPIBase)
	}
	c2, err := parseConn(map[string]any{"api_key": "secret", "api_base": "http://x"})
	if err != nil {
		t.Fatal(err)
	}
	if c2.base != "http://x" {
		t.Errorf("api_base override not applied: %#v", c2)
	}
}

// TestInvokeAgainstFakeNotifiarr drives Invoke end to end against an
// httptest.Server, asserting the passthrough request path and assembled JSON
// body, plus a non-2xx response surfacing as a plugin.Error.
func TestInvokeAgainstFakeNotifiarr(t *testing.T) {
	var gotMethod, gotPath, gotContentType string
	var gotBody map[string]any
	var respBody string
	var respStatus int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
		}
		w.WriteHeader(respStatus)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	conn := map[string]any{"api_key": "key-abc", "api_base": srv.URL}
	p := notifiarrPlugin{}

	t.Run("passthrough path and body", func(t *testing.T) {
		respStatus = 200
		respBody = `{"code":200}`
		res, err := p.Invoke(plugin.InvokeRequest{
			Verb:       "passthrough",
			Connection: conn,
			Options: map[string]any{
				"title":      "Backup finished",
				"message":    "the nightly backup completed",
				"channel_id": "1234567890",
				"color":      "00ff00",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if gotMethod != http.MethodPost || gotPath != "/notification/passthrough/key-abc" {
			t.Fatalf("method/path: %s %s", gotMethod, gotPath)
		}
		if gotContentType != "application/json" {
			t.Errorf("Content-Type: got %q", gotContentType)
		}
		discord, _ := gotBody["discord"].(map[string]any)
		ids, _ := discord["ids"].(map[string]any)
		if ids["channel"] != "1234567890" {
			t.Errorf("discord.ids.channel: %#v", ids)
		}
		text, _ := discord["text"].(map[string]any)
		if text["title"] != "Backup finished" || text["description"] != "the nightly backup completed" {
			t.Errorf("discord.text: %#v", text)
		}
		if discord["color"] != "00ff00" {
			t.Errorf("discord.color: %#v", discord["color"])
		}
		ping, _ := discord["ping"].(map[string]any)
		if ping == nil {
			t.Errorf("discord.ping missing: %#v", discord)
		}
		if res.Outputs["status_code"] != 200 {
			t.Errorf("status_code: %#v", res.Outputs["status_code"])
		}
		result, ok := res.Outputs["result"].(map[string]any)
		if !ok {
			t.Fatalf("result: %#v", res.Outputs["result"])
		}
		if _, ok := result["code"]; !ok {
			t.Errorf("result.code missing: %#v", result)
		}
	})

	t.Run("non-2xx becomes a plugin error", func(t *testing.T) {
		respStatus = 400
		respBody = `{"error":"invalid channel"}`
		_, err := p.Invoke(plugin.InvokeRequest{
			Verb:       "passthrough",
			Connection: conn,
			Options: map[string]any{
				"title": "x", "message": "y", "channel_id": "1",
			},
		})
		if err == nil {
			t.Fatal("expected error for non-2xx response")
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInternalError {
			t.Fatalf("want CodeInternalError, got %v", err)
		}
		if !strings.Contains(pe.Message, "invalid channel") {
			t.Errorf("error message should carry response body: %q", pe.Message)
		}
	})

	t.Run("api escape hatch", func(t *testing.T) {
		respStatus = 200
		respBody = `{"ok":true}`
		res, err := p.Invoke(plugin.InvokeRequest{
			Verb:       "api",
			Connection: conn,
			Options:    map[string]any{"method": "GET", "path": "/user"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if gotMethod != http.MethodGet || gotPath != "/user" {
			t.Fatalf("method/path: %s %s", gotMethod, gotPath)
		}
		result, ok := res.Outputs["result"].(map[string]any)
		if !ok || result["ok"] != true {
			t.Fatalf("result: %#v", res.Outputs["result"])
		}
	})
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// assertJSONEqual compares two values by their JSON encoding, so map/slice
// ordering and typed-vs-any differences (int vs float64) don't cause a false
// mismatch.
func assertJSONEqual(t *testing.T, got, want any) {
	t.Helper()
	gj, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wj, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	var gv, wv any
	_ = json.Unmarshal(gj, &gv)
	_ = json.Unmarshal(wj, &wv)
	gs, _ := json.Marshal(gv)
	ws, _ := json.Marshal(wv)
	if string(gs) != string(ws) {
		t.Errorf("body mismatch:\n got: %s\nwant: %s", gs, ws)
	}
}
