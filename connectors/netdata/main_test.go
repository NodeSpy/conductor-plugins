package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// checkAuth asserts the Bearer header is present with the given token, or
// entirely absent when token is "".
func checkAuth(t *testing.T, r *http.Request, token string) {
	t.Helper()
	got := r.Header.Get("Authorization")
	if token == "" {
		if got != "" {
			t.Errorf("Authorization header: got %q, want none (no api_key configured)", got)
		}
		return
	}
	want := "Bearer " + token
	if got != want {
		t.Errorf("Authorization header: got %q want %q", got, want)
	}
}

func TestInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r, "secret-key")
		if r.URL.Path != "/api/v1/info" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"version": "v1.44.0", "hostname": "box"})
	}))
	defer srv.Close()

	p := newNetdataPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "info",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "secret-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["hostname"] != "box" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

// TestNoAuthHeaderWhenAPIKeyEmpty proves a self-hosted, unauthenticated agent
// is reachable: with no api_key configured, no Authorization header is sent
// at all.
func TestNoAuthHeaderWhenAPIKeyEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r, "")
		writeJSON(w, 200, map[string]any{"hostname": "box"})
	}))
	defer srv.Close()

	p := newNetdataPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "info",
		Connection: map[string]any{"base_url": srv.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCharts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/charts" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"hostname": "box", "charts": map[string]any{"system.cpu": map[string]any{}}})
	}))
	defer srv.Close()

	p := newNetdataPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "charts", Connection: map[string]any{"base_url": srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["hostname"] != "box" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestChart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chart" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("chart"); got != "system.cpu" {
			t.Errorf("chart query: got %q", got)
		}
		writeJSON(w, 200, map[string]any{"id": "system.cpu"})
	}))
	defer srv.Close()

	p := newNetdataPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "chart", Connection: map[string]any{"base_url": srv.URL},
		Options: map[string]any{"chart": "system.cpu"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["id"] != "system.cpu" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestChartMissing(t *testing.T) {
	p := newNetdataPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "chart",
		Connection: map[string]any{"base_url": "http://example.invalid"},
	})
	if err == nil {
		t.Fatal("expected an error for a missing chart option")
	}
}

func TestData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/data" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("chart") != "system.cpu" || q.Get("after") != "-600" || q.Get("before") != "0" ||
			q.Get("points") != "60" || q.Get("dimensions") != "user,system" || q.Get("format") != "json" {
			t.Errorf("query: got %v", q)
		}
		writeJSON(w, 200, map[string]any{"labels": []any{"time", "user", "system"}})
	}))
	defer srv.Close()

	p := newNetdataPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "data", Connection: map[string]any{"base_url": srv.URL},
		Options: map[string]any{
			"chart": "system.cpu", "after": -600, "before": 0, "points": 60,
			"dimensions": []any{"user", "system"}, "format": "json",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAlarmsVerb(t *testing.T) {
	t.Run("default is active-only", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/alarms" {
				t.Errorf("path: got %q", r.URL.Path)
			}
			q := r.URL.Query()
			if q.Get("active") != "true" || q.Has("all") {
				t.Errorf("query: got %v", q)
			}
			writeJSON(w, 200, map[string]any{"alarms": map[string]any{}})
		}))
		defer srv.Close()

		p := newNetdataPlugin()
		if _, err := p.Invoke(plugin.InvokeRequest{Verb: "alarms", Connection: map[string]any{"base_url": srv.URL}}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("all=true requests every configured alarm", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("all") != "true" || q.Has("active") {
				t.Errorf("query: got %v", q)
			}
			writeJSON(w, 200, map[string]any{"alarms": map[string]any{}})
		}))
		defer srv.Close()

		p := newNetdataPlugin()
		_, err := p.Invoke(plugin.InvokeRequest{
			Verb: "alarms", Connection: map[string]any{"base_url": srv.URL},
			Options: map[string]any{"all": true},
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestAlarmLog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/alarm_log" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("after"); got != "1700000000" {
			t.Errorf("after query: got %q", got)
		}
		writeJSON(w, 200, []any{map[string]any{"name": "10min_cpu_usage"}})
	}))
	defer srv.Close()

	p := newNetdataPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "alarm_log", Connection: map[string]any{"base_url": srv.URL},
		Options: map[string]any{"after": 1700000000},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestAlertsV2(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/alerts" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"alerts": []any{}})
	}))
	defer srv.Close()

	p := newNetdataPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "alerts", Connection: map[string]any{"base_url": srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestContexts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/contexts" {
			t.Errorf("path: got %q", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"contexts": map[string]any{}})
	}))
	defer srv.Close()

	p := newNetdataPlugin()
	if _, err := p.Invoke(plugin.InvokeRequest{Verb: "contexts", Connection: map[string]any{"base_url": srv.URL}}); err != nil {
		t.Fatal(err)
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r, "tok")
		if r.URL.Path != "/api/v1/info" || r.Method != http.MethodGet {
			t.Errorf("method/path: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"version": "v1.44.0"})
	}))
	defer srv.Close()

	p := newNetdataPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "api",
		Connection: map[string]any{"base_url": srv.URL, "api_key": "tok"},
		Options:    map[string]any{"path": "/api/v1/info"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestAPIEscapeHatchMissingPath(t *testing.T) {
	p := newNetdataPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "api",
		Connection: map[string]any{"base_url": "http://example.invalid"},
	})
	if err == nil {
		t.Fatal("expected an error for a missing path option")
	}
}

// TestNon2xxIsInternalError proves a non-2xx response becomes a
// CodeInternalError carrying the status and body, never a silently-decoded
// result.
func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := newNetdataPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "info", Connection: map[string]any{"base_url": srv.URL}})
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	perr, ok := err.(*plugin.Error)
	if !ok {
		t.Fatalf("error type: %T (%v)", err, err)
	}
	if perr.Code != plugin.CodeInternalError {
		t.Errorf("code: got %d want %d", perr.Code, plugin.CodeInternalError)
	}
}

func TestMissingBaseURL(t *testing.T) {
	p := newNetdataPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "info", Connection: map[string]any{}})
	if err == nil {
		t.Fatal("expected an error for a missing base_url")
	}
	perr, ok := err.(*plugin.Error)
	if !ok {
		t.Fatalf("error type: %T (%v)", err, err)
	}
	if perr.Code != plugin.CodeInvalidParams {
		t.Errorf("code: got %d want %d", perr.Code, plugin.CodeInvalidParams)
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newNetdataPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb:       "nope",
		Connection: map[string]any{"base_url": "http://example.invalid"},
	})
	if err == nil {
		t.Fatal("expected an error for an unknown verb")
	}
}

// TestDescribe asserts the declared surface: kind, capabilities, every verb,
// and the source event.
func TestDescribe(t *testing.T) {
	d := newNetdataPlugin().Describe()
	if d.Kind != plugin.KindConnector || d.Type != "netdata" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Egress) != 0 {
		t.Fatalf("netdata is self-hosted, but egress is declared: %v", d.Capabilities.Egress)
	}
	want := []string{"info", "charts", "chart", "data", "alarms", "alarm_log", "alerts", "contexts", "api"}
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
	if len(d.Events) == 0 {
		t.Fatal("Describe must declare at least one Event")
	}
	if d.Events[0].Name != "alarm" {
		t.Fatalf("events: %#v", d.Events)
	}
	for _, f := range []string{"statuses", "charts"} {
		if _, ok := d.Events[0].Filters[f]; !ok {
			t.Errorf("alarm event missing filter %q", f)
		}
	}
	if _, required := d.Connection["base_url"]; !required || !d.Connection["base_url"].Required {
		t.Error("base_url must be a required connection field")
	}
	if d.Connection["api_key"].Required {
		t.Error("api_key must be optional (a self-hosted agent is often unauthenticated)")
	}
}

// --- source: the pure alarm emit decision ---

func TestAlarmEvent(t *testing.T) {
	t.Run("CLEAR does not emit", func(t *testing.T) {
		if _, ok := alarmEvent("system.cpu.10min_cpu_usage", map[string]any{
			"name": "10min_cpu_usage", "status": "CLEAR",
		}); ok {
			t.Fatal("expected no event for a CLEAR alarm")
		}
	})

	t.Run("UNDEFINED does not emit", func(t *testing.T) {
		if _, ok := alarmEvent("system.cpu.10min_cpu_usage", map[string]any{
			"name": "10min_cpu_usage", "status": "UNDEFINED",
		}); ok {
			t.Fatal("expected no event for an UNDEFINED alarm")
		}
	})

	t.Run("WARNING emits", func(t *testing.T) {
		ev, ok := alarmEvent("system.cpu.10min_cpu_usage", map[string]any{
			"name": "10min_cpu_usage", "chart": "system.cpu", "family": "cpu",
			"status": "WARNING", "value": 87.5, "units": "%", "info": "cpu usage over 10 minutes",
			"last_status_change": float64(1700000000),
		})
		if !ok {
			t.Fatal("expected an event for a WARNING alarm")
		}
		if ev["event"] != "alarm" || ev["kind"] != "alarm" {
			t.Errorf("event/kind: %#v", ev)
		}
		if ev["dedup"] != "10min_cpu_usage\x00WARNING" {
			t.Errorf("dedup: %#v", ev["dedup"])
		}
		ctx, _ := ev["context"].(map[string]any)
		for k, want := range map[string]any{
			"name": "10min_cpu_usage", "chart": "system.cpu", "family": "cpu",
			"status": "WARNING", "value": 87.5, "units": "%", "info": "cpu usage over 10 minutes",
			"last_status_change": float64(1700000000),
			// plural aliases the documented filters match against
			"statuses": "WARNING", "charts": "system.cpu",
		} {
			if ctx[k] != want {
				t.Errorf("context[%q] = %#v, want %#v", k, ctx[k], want)
			}
		}
	})

	t.Run("CRITICAL emits", func(t *testing.T) {
		ev, ok := alarmEvent("system.disk.out_of_disk_space", map[string]any{
			"name": "out_of_disk_space", "chart": "disk.space", "status": "CRITICAL",
		})
		if !ok {
			t.Fatal("expected an event for a CRITICAL alarm")
		}
		if ev["context"].(map[string]any)["status"] != "CRITICAL" {
			t.Errorf("status: %#v", ev["context"])
		}
	})

	t.Run("falls back to the map key when name is absent", func(t *testing.T) {
		ev, ok := alarmEvent("system.cpu.10min_cpu_usage", map[string]any{"status": "WARNING"})
		if !ok {
			t.Fatal("expected an event")
		}
		if ev["context"].(map[string]any)["name"] != "system.cpu.10min_cpu_usage" {
			t.Errorf("name fallback: %#v", ev["context"])
		}
	})

	// A persistently-raised alarm emits ONCE: the second cycle's identical
	// name+status key is already in the dedup set. A status change (a flap)
	// is a different key, so it re-emits.
	t.Run("dedup on name+status; a flap re-emits", func(t *testing.T) {
		dedup := sourcekit.NewDedup(16)
		entry := map[string]any{"name": "10min_cpu_usage", "status": "WARNING"}
		emitted := 0
		for i := 0; i < 3; i++ {
			ev, ok := alarmEvent("k", entry)
			if ok && dedup.Add(str(ev["dedup"])) {
				emitted++
			}
		}
		if emitted != 1 {
			t.Fatalf("emitted %d times for a steady alarm, want 1", emitted)
		}

		// the flap: status changes to CRITICAL, a new dedup key
		flapped := map[string]any{"name": "10min_cpu_usage", "status": "CRITICAL"}
		ev, ok := alarmEvent("k", flapped)
		if !ok || !dedup.Add(str(ev["dedup"])) {
			t.Fatal("a status change must re-emit")
		}
	})
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]any{"b": nil, "a": nil, "c": nil})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("sortedKeys: %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedKeys: %#v, want %#v", got, want)
		}
	}
}
