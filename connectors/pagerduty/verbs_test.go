package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type pdReq struct {
	method, path, from, auth string
	body                     map[string]any
}

func pdMock(t *testing.T) (*httptest.Server, *[]pdReq) {
	var got []pdReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		got = append(got, pdReq{r.Method, r.URL.RequestURI(), r.Header.Get("From"), r.Header.Get("Authorization"), b})
		inc := map[string]any{"id": "PABC", "incident_number": 42, "status": "triggered", "title": "DB down",
			"urgency": "high", "html_url": "https://x.pagerduty.com/incidents/PABC", "escalation_level": 1, "service": map[string]any{"summary": "api"}}
		switch {
		case strings.HasSuffix(r.URL.Path, "/notes"):
			_ = json.NewEncoder(w).Encode(map[string]any{"note": map[string]any{"id": "N1"}})
		case r.URL.Path == "/incidents" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"incidents": []any{inc}})
		case r.Method == http.MethodPut:
			if inc2, ok := b["incident"].(map[string]any); ok {
				if s, ok := inc2["status"].(string); ok {
					inc["status"] = s
				}
				if l, ok := inc2["escalation_level"].(float64); ok {
					inc["escalation_level"] = l
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"incident": inc})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"incident": inc})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func pdInvoke(srv *httptest.Server, verb string, conn, opts map[string]any) (map[string]any, error) {
	c := map[string]any{"api_token": "tok", "from": "oncall@example.com", "api_base": srv.URL}
	for k, v := range conn {
		c[k] = v
	}
	res, err := pagerduty{}.Invoke(plugin.InvokeRequest{Verb: verb, Connection: c, Options: opts})
	return res.Outputs, err
}

func TestPagerDutyVerbs(t *testing.T) {
	srv, got := pdMock(t)
	out, err := pdInvoke(srv, "acknowledge", nil, map[string]any{"id": "PABC"})
	if err != nil || out["status"] != "acknowledged" || out["number"] != 42 {
		t.Fatalf("acknowledge: %v %v", out, err)
	}
	r := (*got)[0]
	inc, _ := r.body["incident"].(map[string]any)
	if r.method != "PUT" || r.path != "/incidents/PABC" || r.from != "oncall@example.com" || r.auth != "Token token=tok" || inc["status"] != "acknowledged" {
		t.Fatalf("acknowledge request: %+v", r)
	}
	if out, err := pdInvoke(srv, "resolve", nil, map[string]any{"id": "PABC"}); err != nil || out["status"] != "resolved" {
		t.Fatalf("resolve: %v %v", out, err)
	}
	if out, err := pdInvoke(srv, "escalate", nil, map[string]any{"id": "PABC", "level": 2.0}); err != nil || out["escalation_level"] != 2 {
		t.Fatalf("escalate: %v %v", out, err)
	}
	if out, err := pdInvoke(srv, "add_note", nil, map[string]any{"id": "PABC", "content": "investigating"}); err != nil || out["note_id"] != "N1" {
		t.Fatalf("add_note: %v %v", out, err)
	}
	out, err = pdInvoke(srv, "list_incidents", nil, map[string]any{"limit": 500.0})
	if err != nil || out["count"] != 1 {
		t.Fatalf("list: %v %v", out, err)
	}
	last := (*got)[len(*got)-1]
	if !strings.Contains(last.path, "limit=100") || !strings.Contains(last.path, "statuses%5B%5D=triggered") {
		t.Fatalf("list query: %s", last.path)
	}
	if out, err := pdInvoke(srv, "get_incident", map[string]any{"from": ""}, map[string]any{"id": "PABC"}); err != nil || out["title"] != "DB down" {
		t.Fatalf("a read needs no from: %v %v", out, err)
	}
}

func TestPagerDutyVerbRefusals(t *testing.T) {
	srv, got := pdMock(t)
	cases := []struct {
		verb string
		conn map[string]any
		opts map[string]any
	}{
		{"acknowledge", map[string]any{"from": ""}, map[string]any{"id": "PABC"}}, // writes need from
		{"acknowledge", nil, map[string]any{}},                                    // id required
		{"acknowledge", nil, map[string]any{"id": "../users"}},                    // id is a path segment, not a path
		{"escalate", nil, map[string]any{"id": "PABC", "level": 0.0}},
		{"escalate", nil, map[string]any{"id": "PABC", "level": 1.5}},
		{"add_note", nil, map[string]any{"id": "PABC", "content": "  "}},
		{"get_incident", map[string]any{"api_token": ""}, map[string]any{"id": "PABC"}},
		{"explode", nil, map[string]any{"id": "PABC"}},
	}
	t.Setenv("PAGERDUTY_TOKEN", "")
	for _, c := range cases {
		if _, err := pdInvoke(srv, c.verb, c.conn, c.opts); err == nil {
			t.Errorf("%s %v %v accepted", c.verb, c.conn, c.opts)
		}
	}
	if len(*got) != 0 {
		t.Fatalf("refused calls reached the API: %+v", *got)
	}
}
