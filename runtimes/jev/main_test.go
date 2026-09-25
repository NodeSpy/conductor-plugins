package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// The published system_one/v1 response, as TypeSafe's SDK decodes it.
const systemOneResponse = `{
  "model": "jev-1",
  "usage": {"input_tokens": 4210, "output_tokens": 0},
  "answers": {
    "refuted": {"type": "noul", "noul": 0.64},
    "risk": {"type": "choice", "choice": "medium", "confidence": 0.81,
             "probabilities": {"low": 0.12, "medium": 0.81, "high": 0.07}}
  }
}`

func questions() map[string]any {
	return map[string]any{
		"refuted": map[string]any{"type": "noul", "instructions": "contradicted?"},
		"risk":    map[string]any{"type": "choice", "criteria": map[string]any{"low": "a", "medium": "b", "high": "c"}},
	}
}

func TestDescribeIsADecisionRuntime(t *testing.T) {
	d := jevPlugin{}.Describe()
	if d.Kind != plugin.KindRuntime || d.Type != "jev" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Protocols) != 1 || d.Protocols[0] != plugin.ProtocolSystemOneV1 {
		t.Fatalf("protocols = %v", d.Protocols)
	}
	verbs := map[string]bool{}
	for _, v := range d.Verbs {
		verbs[v.Name] = true
	}
	if !verbs[plugin.VerbDecide] || !verbs[plugin.VerbModels] || len(d.Verbs) != 2 {
		t.Fatalf("a decision runtime serves exactly decide and models: %v", verbs)
	}
	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "api.typesafe.ai:443" {
		t.Fatalf("egress: %v", d.Capabilities.Egress)
	}
	if d.Capabilities.Spawns || len(d.Capabilities.Commands) != 0 {
		t.Fatal("the runtime spawns nothing")
	}
	if !d.Connection["api_key"].Required {
		t.Fatal("api_key is required")
	}
}

func TestDecideBodyIsTheV1Request(t *testing.T) {
	c := jevConn{apiKey: "k", base: "x", model: defaultModel}
	body, err := c.decideBody(map[string]any{
		"protocol": plugin.ProtocolSystemOneV1, "state": "the diff", "questions": questions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["model"] != "jev-latest" || body["state"] != "the diff" {
		t.Fatalf("an unset model is the SDK default alias: %v", body)
	}
	if _, has := body["protocol"]; has {
		t.Fatal("protocol is conductor's routing field, not part of the TypeSafe request body")
	}
	body, _ = c.decideBody(map[string]any{
		"protocol": plugin.ProtocolSystemOneV1, "model": "jev-1", "state": map[string]any{"pr": 7}, "questions": questions(),
	})
	if body["model"] != "jev-1" {
		t.Fatalf("the resolved model is sent: %v", body["model"])
	}
	for name, o := range map[string]map[string]any{
		"wrong protocol": {"protocol": "system_one/v2", "state": "s", "questions": questions()},
		"no state":       {"protocol": plugin.ProtocolSystemOneV1, "questions": questions()},
		"no questions":   {"protocol": plugin.ProtocolSystemOneV1, "state": "s"},
	} {
		if _, err := c.decideBody(o); err == nil {
			t.Errorf("%s must be refused before any call", name)
		}
	}
}

// fakeTypeSafe records requests and answers like the real API.
type fakeTypeSafe struct {
	mu     sync.Mutex
	reqs   []*http.Request
	bodies []map[string]any
	status int
	reply  string
}

func (f *fakeTypeSafe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.bodies = append(f.bodies, body)
	status, reply := f.status, f.reply
	f.mu.Unlock()
	if status == 0 {
		status = 200
	}
	if r.URL.Path == "/v1/models" && reply == "" {
		reply = `{"models":[{"name":"jev-1","description":"Jev 1","release_date":"2026-09-01"},{"name":"jev-latest","description":"alias","release_date":"2026-09-01"}]}`
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, reply)
}

func TestDecideRoundTripAgainstTheAPI(t *testing.T) {
	api := &fakeTypeSafe{reply: systemOneResponse}
	srv := httptest.NewServer(api)
	defer srv.Close()
	res, err := jevPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       plugin.VerbDecide,
		Options:    map[string]any{"protocol": plugin.ProtocolSystemOneV1, "model": "jev-1", "state": "the diff", "questions": questions()},
		Connection: map[string]any{"api_key": "sk-test", "api_base": srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := api.reqs[0]
	if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
		t.Fatalf("request: %s %s", r.Method, r.URL.Path)
	}
	if r.Header.Get("Authorization") != "Bearer sk-test" {
		t.Fatalf("auth header: %q", r.Header.Get("Authorization"))
	}
	if api.bodies[0]["model"] != "jev-1" || api.bodies[0]["state"] != "the diff" {
		t.Fatalf("body: %v", api.bodies[0])
	}
	answers := res.Outputs["answers"].(map[string]any)
	if answers["refuted"].(map[string]any)["noul"] != 0.64 {
		t.Fatalf("answers pass through unchanged: %v", answers)
	}
	if res.Outputs["model"] != "jev-1" {
		t.Fatalf("model: %v", res.Outputs["model"])
	}
}

func TestModelsBecomeTheRoster(t *testing.T) {
	api := &fakeTypeSafe{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	res, err := jevPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: plugin.VerbModels, Connection: map[string]any{"api_key": "sk-test", "api_base": srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if api.reqs[0].Method != http.MethodGet || api.reqs[0].URL.Path != "/v1/models" {
		t.Fatalf("request: %s %s", api.reqs[0].Method, api.reqs[0].URL.Path)
	}
	list := res.Outputs["models"].([]any)
	first := list[0].(map[string]any)
	if len(list) != 2 || first["id"] != "jev-1" || first["released"] != "2026-09-01" {
		t.Fatalf("models: %v", list)
	}
}

// An API error surfaces status and a capped body — and never the key, even
// when the upstream echoes it back.
func TestAPIErrorNeverLeaksTheKey(t *testing.T) {
	api := &fakeTypeSafe{status: 401, reply: `{"error":"invalid key sk-secret-123"}` + strings.Repeat("x", 500)}
	srv := httptest.NewServer(api)
	defer srv.Close()
	_, err := jevPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       plugin.VerbDecide,
		Options:    map[string]any{"protocol": plugin.ProtocolSystemOneV1, "state": "s", "questions": questions()},
		Connection: map[string]any{"api_key": "sk-secret-123", "api_base": srv.URL},
	})
	if err == nil {
		t.Fatal("a 401 must be an error so conductor moves to the next candidate")
	}
	if strings.Contains(err.Error(), "sk-secret-123") {
		t.Fatalf("the API key leaked into the error: %v", err)
	}
	if !strings.Contains(err.Error(), "401") || len(err.Error()) > 400 {
		t.Fatalf("the error names the status and caps the body: %v", err)
	}
}

func TestMissingKeyIsRefused(t *testing.T) {
	if _, err := (jevPlugin{}).Invoke(plugin.InvokeRequest{Verb: plugin.VerbModels}); err == nil {
		t.Fatal("no api_key must be refused before any call")
	}
}

// Over the real transport: the built binary describes itself as a decision
// runtime and answers a decide call end to end.
func TestBinaryOverTheWire(t *testing.T) {
	api := &fakeTypeSafe{reply: systemOneResponse}
	srv := httptest.NewServer(api)
	defer srv.Close()
	c := rpctest.Start(t, rpctest.BuildRuntime(t, "jev"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	decl, err := c.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(decl.Protocols) != 1 || decl.Protocols[0] != plugin.ProtocolSystemOneV1 {
		t.Fatalf("protocols over the wire: %v", decl.Protocols)
	}
	out, err := c.Invoke(ctx, plugin.InvokeRequest{
		Instance: "jev", Verb: plugin.VerbDecide,
		Options:    map[string]any{"protocol": plugin.ProtocolSystemOneV1, "state": "the diff", "questions": questions()},
		Connection: map[string]any{"api_key": "sk-test", "api_base": srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out["model"] != "jev-1" {
		t.Fatalf("outputs: %v", out)
	}
}
