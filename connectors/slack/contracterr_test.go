package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// cannedSlack answers every Web API call with the given status/body — the
// error-classification tests need exact control over the HTTP status and
// headers Slack sends, which the other tests' `{"ok":true}`-by-default fake
// doesn't give.
func cannedSlack(t *testing.T, status int, hdr map[string]string, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func asSlackContractError(t *testing.T, err error) *plugin.Error {
	t.Helper()
	if err == nil {
		t.Fatal("got a nil error, want a contract error")
	}
	var pe *plugin.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v (%T) is not a *plugin.Error", err, err)
	}
	return pe
}

// TestSlackRateLimited covers plugin-contract.md §1.11's rate_limited
// (-32013): a 429 (the Web API's rate-limit answer) with retry_after taken
// from Retry-After.
func TestSlackRateLimited(t *testing.T) {
	srv := cannedSlack(t, http.StatusTooManyRequests, map[string]string{"Retry-After": "5"},
		`{"ok":false,"error":"ratelimited"}`)
	p := New()
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "post",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": srv.URL},
		Options:    map[string]any{"channel": "C1", "text": "hi"},
	})
	pe := asSlackContractError(t, err)
	if pe.Code != plugin.CodeRateLimited {
		t.Fatalf("code = %d, want CodeRateLimited; msg=%s", pe.Code, pe.Message)
	}
	wait, _ := pe.Data["retry_after"].(string)
	if wait != "5s" {
		t.Fatalf("retry_after = %#v, want %q", pe.Data["retry_after"], "5s")
	}
	if d, perr := time.ParseDuration(wait); perr != nil || d != 5*time.Second {
		t.Fatalf("retry_after = %q did not parse as 5s: %v", wait, perr)
	}
}

// TestSlackPostToChannelChannelNotFoundIsUpstream covers the adversarial-
// review fix: a bare `post` (no thread_ts) addresses a DESTINATION channel
// no event ever targeted, so channel_not_found must never silently stop the
// run as target_gone — only upstream{retryable:false}.
func TestSlackPostToChannelChannelNotFoundIsUpstream(t *testing.T) {
	srv := cannedSlack(t, http.StatusOK, nil, `{"ok":false,"error":"channel_not_found"}`)
	p := New()
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "post",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": srv.URL},
		Options:    map[string]any{"channel": "C-gone", "text": "hi"},
	})
	pe := asSlackContractError(t, err)
	if pe.Code != plugin.CodeUpstream {
		t.Fatalf("code = %d, want CodeUpstream (never target_gone — a bare post's channel is a destination); msg=%s", pe.Code, pe.Message)
	}
	if retryable, _ := pe.Data["retryable"].(bool); retryable {
		t.Fatalf("retryable = true, want false")
	}
}

// TestSlackThreadReplyChannelNotFoundIsTargetGone covers target_gone
// (-32011): a post WITH thread_ts set addresses the thread's root message —
// the event's own target when it's a reply in that thread — so a gone
// channel/thread here IS target_gone, keyed on the message's target.key.
func TestSlackThreadReplyChannelNotFoundIsTargetGone(t *testing.T) {
	srv := cannedSlack(t, http.StatusOK, nil, `{"ok":false,"error":"channel_not_found"}`)
	p := New()
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "post",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": srv.URL},
		Options:    map[string]any{"channel": "C-gone", "text": "hi", "thread_ts": "123.456"},
	})
	pe := asSlackContractError(t, err)
	if pe.Code != plugin.CodeTargetGone {
		t.Fatalf("code = %d, want CodeTargetGone; msg=%s", pe.Code, pe.Message)
	}
	if got, want := pe.Data["target"], "slack:C-gone:123.456"; got != want {
		t.Fatalf("data.target = %#v, want %q", got, want)
	}
}

// TestSlackMessageNotFoundIsTargetGone: reacting to a message that's gone —
// react always addresses a specific message, so this is always target_gone,
// keyed on that message's target.key.
func TestSlackMessageNotFoundIsTargetGone(t *testing.T) {
	srv := cannedSlack(t, http.StatusOK, nil, `{"ok":false,"error":"message_not_found"}`)
	p := New()
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "react",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": srv.URL},
		Options:    map[string]any{"channel": "C1", "ts": "123.456", "emoji": "+1"},
	})
	pe := asSlackContractError(t, err)
	if pe.Code != plugin.CodeTargetGone {
		t.Fatalf("code = %d, want CodeTargetGone; msg=%s", pe.Code, pe.Message)
	}
	if got, want := pe.Data["target"], "slack:C1:123.456"; got != want {
		t.Fatalf("data.target = %#v, want %q", got, want)
	}
}

// TestSlackOtherErrorsStayGeneric: an error outside the classified set
// (invalid_auth, say) is left as the pre-existing generic upstream answer,
// not forced into one of the specific buckets it doesn't fit.
func TestSlackOtherErrorsStayGeneric(t *testing.T) {
	srv := cannedSlack(t, http.StatusOK, nil, `{"ok":false,"error":"invalid_auth"}`)
	p := New()
	_, err := p.Invoke(plugin.InvokeRequest{
		Instance: "x", Verb: "post",
		Connection: map[string]any{"bot_token": "xoxb-1", "api_base": srv.URL},
		Options:    map[string]any{"channel": "C1", "text": "hi"},
	})
	pe := asSlackContractError(t, err)
	if pe.Code != plugin.CodeUpstream {
		t.Fatalf("code = %d, want CodeUpstream (the generic fallback); msg=%s", pe.Code, pe.Message)
	}
}
