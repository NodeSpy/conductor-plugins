package ghsource

import (
	"context"
	"testing"
)

func TestIsBotActor(t *testing.T) {
	cases := []struct {
		userType, login string
		want            bool
	}{
		{"Bot", "some-app", true},
		{"bot", "some-app", true}, // case-insensitive type
		{"User", "foo[bot]", true},
		{"", "Dependabot[BOT]", true}, // case-insensitive suffix
		{"User", "reviewer", false},
		{"", "alice", false},
		{"User", "botany", false}, // "bot" substring is not the suffix
	}
	for _, c := range cases {
		if got := isBotActor(c.userType, c.login); got != c.want {
			t.Errorf("isBotActor(%q, %q) = %v, want %v", c.userType, c.login, got, c.want)
		}
	}
}

// TestNewCommentAuthorIsBotContext: the published context carries
// author_is_bot from the payload's account type or the [bot] login suffix.
func TestNewCommentAuthorIsBotContext(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	cases := []struct {
		name, payload string
		want          bool
	}{
		{
			"account type Bot",
			`{"action":"created",
			  "repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
			  "issue":{"number":3,"pull_request":{},"user":{"login":"me"}},
			  "comment":{"id":31,"user":{"login":"cursor-app","type":"Bot"},"body":"guard the call site"}}`,
			true,
		},
		{
			"[bot] login with type User",
			`{"action":"created",
			  "repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
			  "issue":{"number":3,"pull_request":{},"user":{"login":"me"}},
			  "comment":{"id":32,"user":{"login":"foo[bot]","type":"User"},"body":"nit"}}`,
			true,
		},
		{
			"ordinary human",
			`{"action":"created",
			  "repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
			  "issue":{"number":3,"pull_request":{},"user":{"login":"me"}},
			  "comment":{"id":33,"user":{"login":"reviewer","type":"User"},"body":"please fix"}}`,
			false,
		},
	}
	for _, c := range cases {
		trs := g.triggersFor(context.Background(), "issue_comment", []byte(c.payload))
		if len(trs) != 1 {
			t.Fatalf("%s: want 1 trigger, got %d", c.name, len(trs))
		}
		if got, _ := trs[0].Context["author_is_bot"].(bool); got != c.want {
			t.Errorf("%s: author_is_bot = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestNewCommentAuthorBotFilter: author_bot gates the variant — false only
// fires for humans, true only for bots, absent for either.
func TestNewCommentAuthorBotFilter(t *testing.T) {
	f := false
	cfg := baseConfig()
	cfg.Rules[0].Actions = as1(map[string]Action{
		"new_comment": {AuthorBot: &f},
	})
	g := newTestIntegration(t, cfg)

	bot := []byte(`{"action":"created",
	  "repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
	  "issue":{"number":3,"pull_request":{},"user":{"login":"me"}},
	  "comment":{"id":40,"user":{"login":"cursor[bot]"},"body":"finding"}}`)
	human := []byte(`{"action":"created",
	  "repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
	  "issue":{"number":3,"pull_request":{},"user":{"login":"me"}},
	  "comment":{"id":41,"user":{"login":"reviewer","type":"User"},"body":"please fix"}}`)

	if trs := g.triggersFor(context.Background(), "issue_comment", bot); len(trs) != 0 {
		t.Fatalf("author_bot: false must not match a bot comment, got %d", len(trs))
	}
	if trs := g.triggersFor(context.Background(), "issue_comment", human); len(trs) != 1 {
		t.Fatalf("author_bot: false must match a human comment, got %d", len(trs))
	}

	// Flipped: only bots.
	tr := true
	cfg.Rules[0].Actions = as1(map[string]Action{
		"new_comment": {AuthorBot: &tr},
	})
	g = newTestIntegration(t, cfg)
	if trs := g.triggersFor(context.Background(), "issue_comment", bot); len(trs) != 1 {
		t.Fatalf("author_bot: true must match a bot comment, got %d", len(trs))
	}
	if trs := g.triggersFor(context.Background(), "issue_comment", human); len(trs) != 0 {
		t.Fatalf("author_bot: true must not match a human comment, got %d", len(trs))
	}
}

// TestChangesRequestedAuthorIsBot: a bot review carries author/author_is_bot
// in context, and the author_bot filter applies.
func TestChangesRequestedAuthorIsBot(t *testing.T) {
	g := newTestIntegration(t, baseConfig())
	review := []byte(`{"action":"submitted",
	  "repository":{"full_name":"acme/widget","name":"widget","owner":{"login":"acme"}},
	  "pull_request":{"number":5,"html_url":"u","head":{"sha":"h","ref":"feat"},"base":{"ref":"main"},"user":{"login":"me"}},
	  "review":{"id":9,"state":"changes_requested","user":{"login":"cursor[bot]","type":"Bot"}}}`)
	trs := g.triggersFor(context.Background(), "pull_request_review", review)
	var cr int
	for _, tr := range trs {
		if tr.Kind == "changes_requested" {
			cr++
			if got, _ := tr.Context["author_is_bot"].(bool); !got {
				t.Error("bot review should publish author_is_bot=true")
			}
			if got := tr.Context["author"]; got != "cursor[bot]" {
				t.Errorf("author = %v, want cursor[bot]", got)
			}
		}
	}
	if cr != 1 {
		t.Fatalf("want 1 changes_requested, got %d (of %d triggers)", cr, len(trs))
	}

	// author_bot: false suppresses the bot review.
	f := false
	cfg := baseConfig()
	cfg.Rules[0].Actions = as1(map[string]Action{
		"changes_requested": {AuthorBot: &f},
	})
	g = newTestIntegration(t, cfg)
	for _, tr := range g.triggersFor(context.Background(), "pull_request_review", review) {
		if tr.Kind == "changes_requested" {
			t.Fatal("author_bot: false must not match a bot review")
		}
	}
}
