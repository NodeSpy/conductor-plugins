package main

import (
	"reflect"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

func TestDescribe(t *testing.T) {
	d := redisPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "redis" {
		t.Fatalf("kind/type = %v/%v", d.Kind, d.Type)
	}
	names := map[string]bool{}
	for _, v := range d.Verbs {
		names[v.Name] = true
	}
	for _, want := range []string{"get", "set", "del", "incr", "expire", "publish", "command"} {
		if !names[want] {
			t.Fatalf("missing verb %q", want)
		}
	}
	if len(d.Events) != 1 || d.Events[0].Name != "message" {
		t.Fatalf("want one message event, got %+v", d.Events)
	}
	if _, ok := d.Events[0].Filters["channels"]; !ok {
		t.Fatal("message event should filter on channels")
	}
}

func TestMessageEvent(t *testing.T) {
	got := messageEvent("cache:user:7", "invalidate", "cache:*")
	want := map[string]any{
		"event": "message",
		"kind":  "message",
		"title": "redis: cache:user:7",
		"context": map[string]any{
			"channel":  "cache:user:7",
			"payload":  "invalidate",
			"pattern":  "cache:*",
			"channels": "cache:user:7",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messageEvent = %#v, want %#v", got, want)
	}
}

func TestNewClientFromURL(t *testing.T) {
	c, err := newClient(map[string]any{"url": "redis://:secret@r.local:6380/2"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.Options().Addr; got != "r.local:6380" {
		t.Fatalf("addr = %q, want r.local:6380", got)
	}
	if got := c.Options().DB; got != 2 {
		t.Fatalf("db = %d, want 2", got)
	}
}

func TestNewClientFromFields(t *testing.T) {
	c, err := newClient(map[string]any{"address": "r.local:6379", "db": 1, "password": "p"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.Options().Addr; got != "r.local:6379" {
		t.Fatalf("addr = %q", got)
	}
	if c.Options().DB != 1 {
		t.Fatalf("db = %d, want 1", c.Options().DB)
	}
}

func TestNewClientBadURL(t *testing.T) {
	if _, err := newClient(map[string]any{"url": "http://nope"}); err == nil {
		t.Fatal("a non-redis url should error")
	}
}

func TestTTLDur(t *testing.T) {
	if ttlDur(30) != 30*time.Second {
		t.Fatal("30 -> 30s")
	}
	if ttlDur(0) != 0 {
		t.Fatal("0 -> no expiry")
	}
	if ttlDur(nil) != 0 {
		t.Fatal("missing -> no expiry")
	}
}

func TestUnknownVerb(t *testing.T) {
	// newClient builds a client but connects lazily, so an unknown verb errors
	// before any network I/O.
	_, err := redisPlugin{}.Invoke(plugin.InvokeRequest{Verb: "nope", Connection: map[string]any{"address": "localhost:6379"}})
	if err == nil {
		t.Fatal("unknown verb should error")
	}
}

func TestStartSourceRequiresChannels(t *testing.T) {
	err := redisPlugin{}.StartSource(nil, plugin.StartSourceRequest{
		Instance: "t",
		Config:   map[string]any{"address": "localhost:6379"},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("StartSource with no subscribe/psubscribe should error")
	}
}
