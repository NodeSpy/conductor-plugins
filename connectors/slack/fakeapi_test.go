package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// jsonDecode decodes an *http.Request's JSON body into out.
func jsonDecode(r *http.Request, out any) error {
	return json.NewDecoder(r.Body).Decode(out)
}

// fakeWebAPI starts a Slack Web API fake that records every call into rec and
// answers each path with responses[path] (a canned JSON body) when set, else
// `{"ok":true}`.
func fakeWebAPI(t *testing.T, rec *fakeSlackAPI, responses map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = jsonDecode(r, &body)
		rec.mu.Lock()
		rec.calls = append(rec.calls, apiCall{path: r.URL.Path, body: body})
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if resp, ok := responses[r.URL.Path]; ok {
			_, _ = w.Write([]byte(resp))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newFakeSlackServerWithTS answers chat.postMessage with the given ts.
func newFakeSlackServerWithTS(t *testing.T, rec *fakeSlackAPI, ts string) *httptest.Server {
	t.Helper()
	return fakeWebAPI(t, rec, map[string]string{
		"/chat.postMessage": `{"ok":true,"ts":"` + ts + `"}`,
	})
}

// newFakeSlackServerWithDM answers conversations.open with the given DM
// channel id and chat.postMessage with the given ts.
func newFakeSlackServerWithDM(t *testing.T, rec *fakeSlackAPI, channel, ts string) *httptest.Server {
	t.Helper()
	return fakeWebAPI(t, rec, map[string]string{
		"/conversations.open": `{"ok":true,"channel":{"id":"` + channel + `"}}`,
		"/chat.postMessage":   `{"ok":true,"ts":"` + ts + `"}`,
	})
}
