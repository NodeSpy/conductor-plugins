package main

import (
	"context"
	"reflect"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

func TestDescribe(t *testing.T) {
	d := pgPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "postgres" {
		t.Fatalf("kind/type = %v/%v", d.Kind, d.Type)
	}
	if d.Connection["url"].Required != true {
		t.Fatal("url should be required")
	}
	names := map[string]bool{}
	for _, v := range d.Verbs {
		names[v.Name] = true
	}
	for _, want := range []string{"query", "exec", "notify"} {
		if !names[want] {
			t.Fatalf("missing verb %q", want)
		}
	}
	for _, v := range d.Verbs {
		if v.Name == "notify" && v.Options["channel"].Scope != "channel" {
			t.Fatal("notify.channel should be scoped 'channel'")
		}
	}
	if len(d.Events) != 1 || d.Events[0].Name != "notification" {
		t.Fatalf("want one notification event, got %+v", d.Events)
	}
	if _, ok := d.Events[0].Filters["channels"]; !ok {
		t.Fatal("notification event should filter on channels")
	}
	if d.Capabilities.Spawns {
		t.Fatal("postgres spawns nothing")
	}
}

func TestNotificationEvent(t *testing.T) {
	got := notificationEvent("row_changed", `{"id":7}`)
	want := map[string]any{
		"event": "notification",
		"kind":  "notification",
		"title": "pg NOTIFY row_changed",
		"context": map[string]any{
			"channel":  "row_changed",
			"payload":  `{"id":7}`,
			"channels": "row_changed",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("notificationEvent = %#v, want %#v", got, want)
	}
}

func TestValidIdent(t *testing.T) {
	for _, ok := range []string{"row_changed", "jobs", "_x", "Chan9"} {
		if !validIdent(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "9leading", "has-dash", "has space", "drop;table", `x"y`} {
		if validIdent(bad) {
			t.Errorf("%q should be rejected (LISTEN injection guard)", bad)
		}
	}
}

func TestAnyList(t *testing.T) {
	if got := anyList([]any{1, "a"}); !reflect.DeepEqual(got, []any{1, "a"}) {
		t.Fatalf("list passthrough = %#v", got)
	}
	if got := anyList("solo"); !reflect.DeepEqual(got, []any{"solo"}) {
		t.Fatalf("single value should wrap, got %#v", got)
	}
	if got := anyList(nil); got != nil {
		t.Fatalf("nil should stay nil, got %#v", got)
	}
}

func TestInvokeRequiresURL(t *testing.T) {
	if _, err := (pgPlugin{}).Invoke(plugin.InvokeRequest{Verb: "query", Options: map[string]any{"sql": "select 1"}}); err == nil {
		t.Fatal("missing url should error")
	}
}

func TestStartSourceValidation(t *testing.T) {
	// no url
	if err := (pgPlugin{}).StartSource(context.Background(), plugin.StartSourceRequest{Config: map[string]any{"listen": []any{"c"}}}, func(any) error { return nil }); err == nil {
		t.Fatal("missing url should error")
	}
	// no channels
	if err := (pgPlugin{}).StartSource(context.Background(), plugin.StartSourceRequest{Config: map[string]any{"url": "postgres://x"}}, func(any) error { return nil }); err == nil {
		t.Fatal("missing listen channels should error")
	}
	// invalid channel name is rejected before any connection is attempted
	if err := (pgPlugin{}).StartSource(context.Background(), plugin.StartSourceRequest{Config: map[string]any{"url": "postgres://x", "listen": []any{"bad;name"}}}, func(any) error { return nil }); err == nil {
		t.Fatal("invalid channel name should error")
	}
}
