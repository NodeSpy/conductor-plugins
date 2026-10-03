package main

import (
	"encoding/json"
	"net"
	"path"
	"strings"
	"testing"

	"github.com/NodeSpy/conductor/pkg/plugin"
)

// TestDeclSemantics proves the declaration is a valid, must-understand
// implementation of the contract: every semantics key this plugin uses is
// one the SDK knows (CheckSemantics), and every declaration hangs together
// internally (ValidateSemantics) — e.g. the ask verb's mint/reads_revision
// cross references, if any existed, would need to resolve.
func TestDeclSemantics(t *testing.T) {
	d := Decl()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if probs := plugin.CheckSemantics(raw); len(probs) > 0 {
		t.Fatalf("CheckSemantics: %v", probs)
	}
	if probs := plugin.ValidateSemantics(d); len(probs) > 0 {
		t.Fatalf("ValidateSemantics: %v", probs)
	}
}

// TestDeclBasics proves the shape the task asked to keep working unchanged:
// the connection keys, event names, and verb names the builtin connector
// declared.
func TestDeclBasics(t *testing.T) {
	d := Decl()
	if d.Type != "slack" {
		t.Fatalf("type = %q", d.Type)
	}
	if d.Kind != plugin.KindConnector {
		t.Fatalf("kind = %q", d.Kind)
	}
	for _, key := range []string{"app_token", "bot_token", "webhook_url", "api_base"} {
		if _, ok := d.Connection[key]; !ok {
			t.Errorf("connection missing %q", key)
		}
	}
	events := map[string]plugin.Event{}
	for _, e := range d.Events {
		events[e.Name] = e
	}
	verbs := map[string]plugin.Verb{}
	for _, v := range d.Verbs {
		verbs[v.Name] = v
	}
	wantEvents := []string{"app_mention", "reaction_added", "slash_command", "message_shortcut", "reply"}
	for _, name := range wantEvents {
		if _, ok := events[name]; !ok {
			t.Errorf("missing event %q", name)
		}
	}
	wantVerbs := []string{"post", "react", "ask", "thread", "download"}
	for _, name := range wantVerbs {
		if _, ok := verbs[name]; !ok {
			t.Errorf("missing verb %q", name)
		}
	}
	ask := verbs["ask"]
	if !ask.Ask || ask.Semantics == nil || ask.Semantics.OpensConversation == nil {
		t.Fatalf("ask verb must declare Ask + opens_conversation: %+v", ask)
	}
	post := verbs["post"]
	if post.Semantics == nil || !post.Semantics.ConversationPost {
		t.Fatalf("post verb must declare conversation_post")
	}
	reply := events["reply"]
	if reply.Semantics == nil || reply.Semantics.ConversationReply == nil {
		t.Fatalf("reply event must declare conversation_reply")
	}
	mention := events["app_mention"]
	if mention.Semantics == nil || mention.Semantics.Target == nil || mention.Semantics.Author == nil {
		t.Fatalf("app_mention must declare target + author semantics")
	}
	var scopedChannel, scopedUser bool
	for _, s := range mention.Semantics.Target.Scope {
		if s.Dimension == "channel" {
			scopedChannel = true
		}
		if s.Dimension == "user" {
			scopedUser = true
		}
	}
	if !scopedChannel || !scopedUser {
		t.Fatalf("app_mention target must scope channel and user: %+v", mention.Semantics.Target.Scope)
	}
}

// TestEgressCoversSlackSubdomains is the regression test for finding #11:
// the Web API lives on the bare slack.com host, but Socket Mode's WSS URL
// (apps.connections.open returns something like wss-primary.slack.com) and
// Slack's file download/upload URLs are on subdomains — a declared egress of
// only "slack.com:443" would have conductor's sandbox block both.
func TestEgressCoversSlackSubdomains(t *testing.T) {
	egress := Decl().Capabilities.Egress
	for _, hostport := range []string{"slack.com:443", "wss-primary.slack.com:443", "files.slack.com:443"} {
		if !egressAllows(egress, hostport) {
			t.Errorf("declared egress %v does not allow %q", egress, hostport)
		}
	}
	// A host under a different domain must still be refused.
	if egressAllows(egress, "evil.example:443") {
		t.Errorf("declared egress %v wrongly allows an unrelated host", egress)
	}
}

// egressAllows mirrors conductor's internal/sandbox.go EgressAllowed
// matching semantics (a host glob via path.Match, port defaulting to 443)
// closely enough for this test — that package is internal to the conductor
// module and not importable from here.
func egressAllows(allow []string, hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, "443"
	}
	for _, pat := range allow {
		ph, pp, err := net.SplitHostPort(pat)
		if err != nil {
			ph, pp = pat, "443"
		}
		if pp != port {
			continue
		}
		if ok, _ := path.Match(strings.ToLower(ph), strings.ToLower(host)); ok {
			return true
		}
	}
	return false
}
