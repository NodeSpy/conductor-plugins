package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

func TestValidateMissingToken(t *testing.T) {
	p := New()
	res, err := p.Validate(context.Background(), plugin.ValidateRequest{Config: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) == 0 {
		t.Fatal("expected a problem: neither bot_token nor webhook_url is set")
	}
}

func TestValidateTriggersNeedAppToken(t *testing.T) {
	p := New()
	res, err := p.Validate(context.Background(), plugin.ValidateRequest{
		Config:   map[string]any{"bot_token": "xoxb-1"},
		Triggers: []plugin.SourceTrigger{{ID: "t1", Event: "app_mention"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawAppToken bool
	for _, p := range res.Problems {
		if p.Path == "app_token" {
			sawAppToken = true
		}
	}
	if !sawAppToken {
		t.Fatalf("expected an app_token problem when triggers are configured: %+v", res.Problems)
	}
}

func TestValidateFeedbackOptions(t *testing.T) {
	p := New()
	res, err := p.Validate(context.Background(), plugin.ValidateRequest{
		Config: map[string]any{"bot_token": "xoxb-1", "app_token": "xapp-1"},
		Triggers: []plugin.SourceTrigger{
			{ID: "t1", Event: "app_mention", Options: map[string]any{
				"ack":     map[string]any{"react": "eyes"},
				"on_done": map[string]any{},
				"on_fail": map[string]any{"react": "x", "ephemeral": true},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"t1.options.on_done": false, "t1.options.on_fail": false,
	}
	for _, pr := range res.Problems {
		switch pr.Path {
		case "triggers[0].options.on_done":
			if pr.Message == "" {
				t.Fatalf("on_done: empty block should be refused, got %+v", pr)
			}
			want["t1.options.on_done"] = true
		case "triggers[0].options.on_fail":
			want["t1.options.on_fail"] = true
		case "triggers[0].options.ack":
			t.Fatalf("ack: {react: eyes} is valid and should not be a problem: %+v", pr)
		}
	}
	for k, got := range want {
		if !got {
			t.Fatalf("missing expected problem for %s: %+v", k, res.Problems)
		}
	}
}

func TestValidateFormNeedsUsers(t *testing.T) {
	p := New()
	res, err := p.Validate(context.Background(), plugin.ValidateRequest{
		Config: map[string]any{"bot_token": "xoxb-1", "app_token": "xapp-1"},
		Triggers: []plugin.SourceTrigger{{
			ID: "t1", Event: "app_mention",
			Options: map[string]any{"form": map[string]any{"fields": []any{
				map[string]any{"name": "env", "type": "text"},
			}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 1 {
		t.Fatalf("want exactly one problem (missing users:), got %+v", res.Problems)
	}
}

func TestValidateFormWithUsersOK(t *testing.T) {
	p := New()
	st := plugin.SourceTrigger{
		ID: "t1", Event: "app_mention",
		Options: map[string]any{"form": map[string]any{"fields": []any{
			map[string]any{"name": "env", "type": "text"},
		}}},
	}
	st.Filter = mustFilterJSON(t, map[string]any{"users": []any{"U1"}})
	res, err := p.Validate(context.Background(), plugin.ValidateRequest{
		Config:   map[string]any{"bot_token": "xoxb-1", "app_token": "xapp-1"},
		Triggers: []plugin.SourceTrigger{st},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 0 {
		t.Fatalf("a form trigger with users: should validate clean, got %+v", res.Problems)
	}
}

func TestValidateFormAnyUserOK(t *testing.T) {
	p := New()
	res, err := p.Validate(context.Background(), plugin.ValidateRequest{
		Config: map[string]any{"bot_token": "xoxb-1", "app_token": "xapp-1"},
		Triggers: []plugin.SourceTrigger{{
			ID: "t1", Event: "app_mention",
			Options: map[string]any{
				"any_user": true,
				"form": map[string]any{"fields": []any{
					map[string]any{"name": "env", "type": "text"},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 0 {
		t.Fatalf("any_user: true should validate clean, got %+v", res.Problems)
	}
}

func TestValidatePlainMentionNeedsNoUsers(t *testing.T) {
	p := New()
	res, err := p.Validate(context.Background(), plugin.ValidateRequest{
		Config:   map[string]any{"bot_token": "xoxb-1", "app_token": "xapp-1"},
		Triggers: []plugin.SourceTrigger{{ID: "t1", Event: "app_mention"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 0 {
		t.Fatalf("a plain mention trigger needs no users:, got %+v", res.Problems)
	}
}

func TestValidateBareShortcutNeedsUsers(t *testing.T) {
	p := New()
	res, err := p.Validate(context.Background(), plugin.ValidateRequest{
		Config:   map[string]any{"bot_token": "xoxb-1", "app_token": "xapp-1"},
		Triggers: []plugin.SourceTrigger{{ID: "t1", Event: "message_shortcut"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 1 {
		t.Fatalf("want one problem (missing users:), got %+v", res.Problems)
	}
}

func mustFilterJSON(t *testing.T, surface any) []byte {
	t.Helper()
	f, err := sourcekit.ParseFilter(surface)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
