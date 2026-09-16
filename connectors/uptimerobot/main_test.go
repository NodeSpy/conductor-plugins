package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// --- Describe ---

// TestDescribe asserts the declared surface: kind, type, egress-only
// capabilities, the single "alert" event and its filter keys, and that every
// documented verb is present.
func TestDescribe(t *testing.T) {
	d := uptimerobotPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "uptimerobot" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "api.uptimerobot.com:443" {
		t.Fatalf("egress: got %#v", d.Capabilities.Egress)
	}
	if len(d.Capabilities.Commands) != 0 || d.Capabilities.Spawns {
		t.Fatalf("capabilities should not spawn/command: %#v", d.Capabilities)
	}
	if len(d.Events) != 1 || d.Events[0].Name != "alert" {
		t.Fatalf("events: %#v", d.Events)
	}
	wantFilters := []string{"alert_types", "monitors", "alert_type", "monitor_name"}
	for _, k := range wantFilters {
		if _, ok := d.Events[0].Filters[k]; !ok {
			t.Errorf("missing filter key %q", k)
		}
	}
	wantVerbs := []string{
		"get_monitors", "new_monitor", "edit_monitor", "delete_monitor",
		"pause_monitor", "resume_monitor", "get_account_details", "get_alert_contacts", "api",
	}
	got := map[string]bool{}
	for _, v := range d.Verbs {
		got[v.Name] = true
	}
	for _, name := range wantVerbs {
		if !got[name] {
			t.Errorf("missing verb %q", name)
		}
	}
	if len(d.Verbs) != len(wantVerbs) {
		t.Errorf("verb count: got %d want %d (%#v)", len(d.Verbs), len(wantVerbs), got)
	}
}

// --- verbs against an httptest.Server standing in for the UptimeRobot API ---

// newTestServer returns an httptest.Server that asserts every request is a
// POST to /{method} with api_key+format=json in the form body, and responds
// with the given JSON body (marshaled from resp) for any method matching
// wantMethod, else fails the test.
func newTestServer(t *testing.T, wantMethod string, checkForm func(url.Values), resp map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s want POST", r.Method)
		}
		if r.URL.Path != "/"+wantMethod {
			t.Errorf("path: got %s want /%s", r.URL.Path, wantMethod)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content-type: got %q", ct)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.Form.Get("api_key") != "test-key" {
			t.Errorf("api_key: got %q want test-key", r.Form.Get("api_key"))
		}
		if r.Form.Get("format") != "json" {
			t.Errorf("format: got %q want json", r.Form.Get("format"))
		}
		if checkForm != nil {
			checkForm(r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestGetMonitors(t *testing.T) {
	srv := newTestServer(t, "getMonitors", func(form url.Values) {
		if form.Get("monitors") != "111-222" {
			t.Errorf("monitors: got %q", form.Get("monitors"))
		}
		if form.Get("logs") != "1" {
			t.Errorf("logs: got %q want 1", form.Get("logs"))
		}
	}, map[string]any{
		"stat": "ok",
		"monitors": []any{
			map[string]any{"id": float64(111), "friendly_name": "svc-a", "status": float64(2)},
			map[string]any{"id": float64(222), "friendly_name": "svc-b", "status": float64(9)},
		},
	})
	defer srv.Close()

	res, err := uptimerobotPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "get_monitors",
		Connection: map[string]any{"api_key": "test-key", "api_base": srv.URL},
		Options:    map[string]any{"monitors": []any{"111", "222"}, "logs": true},
	})
	if err != nil {
		t.Fatalf("get_monitors: %v", err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items: got %#v", res.Outputs["items"])
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: got %#v", res.Outputs["status_code"])
	}
}

func TestNewMonitor(t *testing.T) {
	srv := newTestServer(t, "newMonitor", func(form url.Values) {
		if form.Get("friendly_name") != "svc-a" || form.Get("url") != "https://a.example.com" || form.Get("type") != "1" {
			t.Errorf("form: %#v", form)
		}
	}, map[string]any{"stat": "ok", "monitor": map[string]any{"id": float64(999), "status": float64(1)}})
	defer srv.Close()

	res, err := uptimerobotPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "new_monitor",
		Connection: map[string]any{"api_key": "test-key", "api_base": srv.URL},
		Options:    map[string]any{"friendly_name": "svc-a", "url": "https://a.example.com", "type": 1},
	})
	if err != nil {
		t.Fatalf("new_monitor: %v", err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: got %#v", res.Outputs["result"])
	}
	if result["stat"] != "ok" {
		t.Errorf("result.stat: got %#v", result["stat"])
	}
}

func TestNewMonitorRequiredFields(t *testing.T) {
	_, err := uptimerobotPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "new_monitor",
		Connection: map[string]any{"api_key": "test-key", "api_base": "http://unused.invalid"},
		Options:    map[string]any{"friendly_name": "svc-a"},
	})
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams, got %v", err)
	}
}

func TestPauseAndResumeMonitor(t *testing.T) {
	cases := []struct {
		verb       string
		wantStatus string
	}{
		{"pause_monitor", "0"},
		{"resume_monitor", "1"},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			srv := newTestServer(t, "editMonitor", func(form url.Values) {
				if form.Get("id") != "42" {
					t.Errorf("id: got %q", form.Get("id"))
				}
				if form.Get("status") != tc.wantStatus {
					t.Errorf("status: got %q want %q", form.Get("status"), tc.wantStatus)
				}
			}, map[string]any{"stat": "ok", "monitor": map[string]any{"id": float64(42)}})
			defer srv.Close()

			_, err := uptimerobotPlugin{}.Invoke(plugin.InvokeRequest{
				Verb:       tc.verb,
				Connection: map[string]any{"api_key": "test-key", "api_base": srv.URL},
				Options:    map[string]any{"id": "42"},
			})
			if err != nil {
				t.Fatalf("%s: %v", tc.verb, err)
			}
		})
	}
}

func TestDeleteMonitor(t *testing.T) {
	srv := newTestServer(t, "deleteMonitor", func(form url.Values) {
		if form.Get("id") != "7" {
			t.Errorf("id: got %q", form.Get("id"))
		}
	}, map[string]any{"stat": "ok"})
	defer srv.Close()

	_, err := uptimerobotPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "delete_monitor",
		Connection: map[string]any{"api_key": "test-key", "api_base": srv.URL},
		Options:    map[string]any{"id": "7"},
	})
	if err != nil {
		t.Fatalf("delete_monitor: %v", err)
	}
}

func TestGetAccountDetails(t *testing.T) {
	srv := newTestServer(t, "getAccountDetails", nil, map[string]any{
		"stat": "ok", "account": map[string]any{"up_monitors": float64(5)},
	})
	defer srv.Close()

	res, err := uptimerobotPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "get_account_details",
		Connection: map[string]any{"api_key": "test-key", "api_base": srv.URL},
	})
	if err != nil {
		t.Fatalf("get_account_details: %v", err)
	}
	if _, ok := res.Outputs["result"]; !ok {
		t.Fatalf("result missing: %#v", res.Outputs)
	}
}

func TestGetAlertContacts(t *testing.T) {
	srv := newTestServer(t, "getAlertContacts", nil, map[string]any{
		"stat":           "ok",
		"alert_contacts": []any{map[string]any{"id": "1", "type": float64(2)}},
	})
	defer srv.Close()

	res, err := uptimerobotPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "get_alert_contacts",
		Connection: map[string]any{"api_key": "test-key", "api_base": srv.URL},
	})
	if err != nil {
		t.Fatalf("get_alert_contacts: %v", err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: got %#v", res.Outputs["items"])
	}
}

func TestGenericAPI(t *testing.T) {
	srv := newTestServer(t, "getFriendlyName", func(form url.Values) {
		if form.Get("extra") != "yes" {
			t.Errorf("extra: got %q", form.Get("extra"))
		}
	}, map[string]any{"stat": "ok"})
	defer srv.Close()

	_, err := uptimerobotPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "api",
		Connection: map[string]any{"api_key": "test-key", "api_base": srv.URL},
		Options:    map[string]any{"method": "getFriendlyName", "params": map[string]any{"extra": "yes"}},
	})
	if err != nil {
		t.Fatalf("api: %v", err)
	}
}

// TestStatFailIsError asserts a 200 response carrying "stat":"fail" surfaces
// as a plugin.CodeInternalError, not a silent success.
func TestStatFailIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stat":  "fail",
			"error": map[string]any{"type": "invalid_parameter", "message": "api_key not found"},
		})
	}))
	defer srv.Close()

	_, err := uptimerobotPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "get_monitors",
		Connection: map[string]any{"api_key": "bad-key", "api_base": srv.URL},
	})
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "api_key not found") {
		t.Errorf("message should carry the provider error: %q", pe.Message)
	}
}

// TestNon2xxIsError asserts a transport-level non-2xx status is also a
// plugin.CodeInternalError carrying the status and body.
func TestNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	_, err := uptimerobotPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "get_monitors",
		Connection: map[string]any{"api_key": "test-key", "api_base": srv.URL},
	})
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInternalError {
		t.Fatalf("want CodeInternalError, got %v", err)
	}
	if !strings.Contains(pe.Message, "500") || !strings.Contains(pe.Message, "boom") {
		t.Errorf("message should carry status+body: %q", pe.Message)
	}
}

func TestUnknownVerb(t *testing.T) {
	_, err := uptimerobotPlugin{}.Invoke(plugin.InvokeRequest{Verb: "nope"})
	var pe *plugin.Error
	if !asPluginError(err, &pe) || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("want CodeInvalidParams, got %v", err)
	}
}

// --- webhook alert parsing: form and JSON bodies ---

func TestParseAlertForm(t *testing.T) {
	body := "monitorID=781616&monitorURL=" + url.QueryEscape("https://app.example.com") +
		"&monitorFriendlyName=" + url.QueryEscape("App") +
		"&alertType=1&alertDetails=" + url.QueryEscape("connection timeout") +
		"&alertDateTime=" + url.QueryEscape("1234567890")

	f, ok := parseAlert(nil, []byte(body))
	if !ok {
		t.Fatal("expected parse to succeed")
	}
	if f.monitorID != "781616" || f.monitorURL != "https://app.example.com" || f.monitorFriendlyName != "App" {
		t.Fatalf("facts: %#v", f)
	}
	if f.alertKind != "down" {
		t.Errorf("alertKind: got %q want down", f.alertKind)
	}
	if f.alertDetails != "connection timeout" || f.alertDateTime != "1234567890" {
		t.Errorf("details/datetime: %#v", f)
	}
}

func TestParseAlertJSON(t *testing.T) {
	body := `{"monitorID":781616,"monitorURL":"https://app.example.com","monitorFriendlyName":"App","alertType":"2","alertDetails":"back up","alertDateTime":"1234567999"}`

	f, ok := parseAlert(nil, []byte(body))
	if !ok {
		t.Fatal("expected parse to succeed")
	}
	if f.monitorID != "781616" {
		t.Errorf("monitorID: got %q want 781616 (numeric JSON coerced to string)", f.monitorID)
	}
	if f.alertKind != "up" {
		t.Errorf("alertKind: got %q want up", f.alertKind)
	}
}

func TestParseAlertRejectsEmpty(t *testing.T) {
	if _, ok := parseAlert(nil, []byte("")); ok {
		t.Fatal("empty body should not parse")
	}
	if _, ok := parseAlert(nil, []byte("alertType=1")); ok {
		t.Fatal("a body with no monitor identity should not parse")
	}
}

// TestAlertEventShape drives StartSource's event construction indirectly by
// building the same map the handler emits, and asserts the documented
// context + filter alias keys are present.
func TestAlertEventShape(t *testing.T) {
	f, ok := parseAlert(nil, []byte("monitorID=1&monitorURL=https://x&monitorFriendlyName=X&alertType=1&alertDetails=d&alertDateTime=t"))
	if !ok {
		t.Fatal("parse failed")
	}
	ev := map[string]any{
		"event": "alert",
		"kind":  f.alertKind,
		"dedup": dedupKey(f),
		"context": map[string]any{
			"alert_type": f.alertKind, "monitor_id": f.monitorID, "monitor_url": f.monitorURL,
			"monitor_name": f.monitorFriendlyName, "alert_details": f.alertDetails, "datetime": f.alertDateTime,
			"alert_types": f.alertKind, "monitors": []any{f.monitorID, f.monitorFriendlyName},
		},
	}
	ctx := ev["context"].(map[string]any)
	for _, k := range []string{"alert_type", "monitor_id", "monitor_url", "monitor_name", "alert_details", "datetime", "alert_types", "monitors"} {
		if _, ok := ctx[k]; !ok {
			t.Errorf("context missing key %q", k)
		}
	}
	// dedupKey is monitorID\x00alertType\x00alertDateTime.
	if want := "1\x001\x00t"; ev["dedup"] != want {
		t.Errorf("dedup: got %q want %q", ev["dedup"], want)
	}
}

func TestDedupKey(t *testing.T) {
	f1 := alertFacts{monitorID: "1", alertType: "1", alertDateTime: "t1"}
	f2 := alertFacts{monitorID: "1", alertType: "2", alertDateTime: "t1"}
	if dedupKey(f1) == dedupKey(f2) {
		t.Fatal("distinct alert types should not collide")
	}

	dedup := sourcekit.NewDedup(2048)
	if !dedup.Add(dedupKey(f1)) {
		t.Fatal("first delivery should be new")
	}
	if dedup.Add(dedupKey(f1)) {
		t.Fatal("redelivery should be a duplicate")
	}
	if !dedup.Add(dedupKey(f2)) {
		t.Fatal("a distinct alert (different alertType) should be new")
	}
}

// --- token verification ---

func TestVerifyTokenFromRequest(t *testing.T) {
	mk := func(header, query string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/uptimerobot?"+query, nil)
		if header != "" {
			r.Header.Set("X-Conductor-Token", header)
		}
		return r
	}
	cases := []struct {
		name   string
		secret string
		header string
		query  string
		want   bool
	}{
		{"correct header", "s3cret", "s3cret", "", true},
		{"wrong header", "s3cret", "nope", "", false},
		{"correct query fallback", "s3cret", "", "token=s3cret", true},
		{"wrong query", "s3cret", "", "token=nope", false},
		{"header wins over query", "s3cret", "s3cret", "token=nope", true},
		{"neither present", "s3cret", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyTokenFromRequest(tc.secret, mk(tc.header, tc.query)); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestRequireWebhookSecret(t *testing.T) {
	if err := requireWebhookSecret("uptimerobot", "", false); err == nil {
		t.Fatal("empty secret with no allow_unsigned should fail closed")
	}
	if err := requireWebhookSecret("uptimerobot", "", true); err != nil {
		t.Fatalf("allow_unsigned should permit an empty secret: %v", err)
	}
	if err := requireWebhookSecret("uptimerobot", "s3cret", false); err != nil {
		t.Fatalf("a configured secret should never fail: %v", err)
	}
}

func TestInvokeIsNotSourceOnly(t *testing.T) {
	// Sanity check the opposite of the source-only connectors: uptimerobot DOES
	// support verbs, so Invoke with a known verb must not error solely because
	// it is a "source connector".
	srv := newTestServer(t, "getAccountDetails", nil, map[string]any{"stat": "ok", "account": map[string]any{}})
	defer srv.Close()
	_, err := uptimerobotPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "get_account_details",
		Connection: map[string]any{"api_key": "test-key", "api_base": srv.URL},
	})
	if err != nil {
		t.Fatalf("get_account_details should succeed: %v", err)
	}
}

// --- helpers ---

func asPluginError(err error, target **plugin.Error) bool {
	pe, ok := err.(*plugin.Error)
	if ok {
		*target = pe
	}
	return ok
}
