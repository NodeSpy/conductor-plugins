package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// cannedDiscord answers every REST call with the given status/body.
func cannedDiscord(t *testing.T, status int, hdr map[string]string, body string) *httptest.Server {
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

func asDiscordContractError(t *testing.T, err error) *plugin.Error {
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

// TestDiscordRateLimited covers plugin-contract.md §1.11's rate_limited
// (-32013): Discord's 429 body always carries its own retry_after (seconds,
// possibly fractional), preferred over any Retry-After header.
func TestDiscordRateLimited(t *testing.T) {
	srv := cannedDiscord(t, http.StatusTooManyRequests, nil,
		`{"message":"You are being rate limited.","retry_after":1.5,"global":false}`)
	_, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "post",
		Connection: map[string]any{"bot_token": "tok", "api_base": srv.URL},
		Options:    map[string]any{"channel": "C1", "text": "hi"},
	})
	pe := asDiscordContractError(t, err)
	if pe.Code != plugin.CodeRateLimited {
		t.Fatalf("code = %d, want CodeRateLimited; msg=%s", pe.Code, pe.Message)
	}
	wait, _ := pe.Data["retry_after"].(string)
	d, perr := time.ParseDuration(wait)
	if perr != nil || d != 1500*time.Millisecond {
		t.Fatalf("retry_after = %q, want 1.5s worth of duration: %v", wait, perr)
	}
}

// TestDiscordRateLimitedFallsBackToHeader covers the case where the body
// carries no retry_after but the Retry-After header does.
func TestDiscordRateLimitedFallsBackToHeader(t *testing.T) {
	srv := cannedDiscord(t, http.StatusTooManyRequests, map[string]string{"Retry-After": "2"}, `{}`)
	_, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "post",
		Connection: map[string]any{"bot_token": "tok", "api_base": srv.URL},
		Options:    map[string]any{"channel": "C1", "text": "hi"},
	})
	pe := asDiscordContractError(t, err)
	if pe.Code != plugin.CodeRateLimited {
		t.Fatalf("code = %d, want CodeRateLimited; msg=%s", pe.Code, pe.Message)
	}
	if got := pe.Data["retry_after"]; got != "2s" {
		t.Fatalf("retry_after = %#v, want %q", got, "2s")
	}
}

// TestDiscordUnknownChannelIsTargetGone covers target_gone (-32011): the
// channel a post/ask addressed is gone (Discord error code 10003).
func TestDiscordUnknownChannelIsTargetGone(t *testing.T) {
	srv := cannedDiscord(t, http.StatusNotFound, nil, `{"message":"Unknown Channel","code":10003}`)
	_, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "post",
		Connection: map[string]any{"bot_token": "tok", "api_base": srv.URL},
		Options:    map[string]any{"channel": "C-gone", "text": "hi"},
	})
	pe := asDiscordContractError(t, err)
	if pe.Code != plugin.CodeTargetGone {
		t.Fatalf("code = %d, want CodeTargetGone; msg=%s", pe.Code, pe.Message)
	}
}

// TestDiscordOtherErrorsStayGeneric: an error outside the classified set is
// left as the pre-existing generic internal-error answer.
func TestDiscordOtherErrorsStayGeneric(t *testing.T) {
	srv := cannedDiscord(t, http.StatusForbidden, nil, `{"message":"Missing Access","code":50001}`)
	_, err := discordPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:       "post",
		Connection: map[string]any{"bot_token": "tok", "api_base": srv.URL},
		Options:    map[string]any{"channel": "C1", "text": "hi"},
	})
	pe := asDiscordContractError(t, err)
	if pe.Code != plugin.CodeInternalError {
		t.Fatalf("code = %d, want CodeInternalError (the generic fallback); msg=%s", pe.Code, pe.Message)
	}
}
