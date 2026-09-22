package main

import (
	"context"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

func invoke(t *testing.T, verb string, opts map[string]any) plugin.InvokeResult {
	t.Helper()
	res, err := testPlugin{}.Invoke(plugin.InvokeRequest{Instance: "t", Verb: verb, Options: opts})
	if err != nil {
		t.Fatalf("%s: %v", verb, err)
	}
	return res
}

func TestDescribe(t *testing.T) {
	d := testPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "test" {
		t.Fatalf("kind/type = %v/%v", d.Kind, d.Type)
	}
	names := map[string]bool{}
	for _, v := range d.Verbs {
		names[v.Name] = true
	}
	for _, want := range []string{"ping", "echo", "sleep", "fail", "counter", "random", "now"} {
		if !names[want] {
			t.Fatalf("missing verb %q", want)
		}
	}
	if len(d.Events) != 1 || d.Events[0].Name != "tick" {
		t.Fatalf("want one tick event, got %+v", d.Events)
	}
	if d.Capabilities.Spawns {
		t.Fatal("test connector spawns nothing")
	}
}

func TestPing(t *testing.T) {
	res := invoke(t, "ping", map[string]any{"message": "hi"})
	if res.Outputs["pong"] != true || res.Outputs["message"] != "hi" || res.Outputs["instance"] != "t" {
		t.Fatalf("ping = %#v", res.Outputs)
	}
	// default message
	if got := invoke(t, "ping", nil).Outputs["message"]; got != "pong" {
		t.Fatalf("default ping message = %v", got)
	}
}

func TestEcho(t *testing.T) {
	res := invoke(t, "echo", map[string]any{"data": []any{1, 2}, "extra": "x"})
	if !reflect.DeepEqual(res.Outputs["data"], []any{1, 2}) {
		t.Fatalf("echo data = %#v", res.Outputs["data"])
	}
	recv := res.Outputs["received"].(map[string]any)
	if recv["extra"] != "x" {
		t.Fatalf("echo should return all options, got %#v", recv)
	}
}

func TestFailAlwaysErrors(t *testing.T) {
	_, err := testPlugin{}.Invoke(plugin.InvokeRequest{Verb: "fail", Options: map[string]any{"message": "boom"}})
	if err == nil {
		t.Fatal("fail must return an error")
	}
	if e, ok := err.(*plugin.Error); ok {
		if e.Message != "boom" {
			t.Fatalf("fail message = %q", e.Message)
		}
		if e.Code != plugin.CodeInternalError {
			t.Fatalf("default fail code = %d, want internal", e.Code)
		}
	}
	// invalid_params code
	_, err = testPlugin{}.Invoke(plugin.InvokeRequest{Verb: "fail", Options: map[string]any{"code": "invalid_params"}})
	if e, ok := err.(*plugin.Error); ok && e.Code != plugin.CodeInvalidParams {
		t.Fatalf("fail code = %d, want invalid_params", e.Code)
	}
}

func TestCounter(t *testing.T) {
	invoke(t, "counter", map[string]any{"reset": true})
	if got := invoke(t, "counter", nil).Outputs["count"]; got != int64(1) {
		t.Fatalf("first counter = %v, want 1", got)
	}
	if got := invoke(t, "counter", nil).Outputs["count"]; got != int64(2) {
		t.Fatalf("second counter = %v, want 2", got)
	}
	if got := invoke(t, "counter", map[string]any{"reset": true}).Outputs["count"]; got != int64(0) {
		t.Fatalf("reset counter = %v, want 0", got)
	}
}

func TestRandom(t *testing.T) {
	res := invoke(t, "random", map[string]any{"max": 10})
	uuid, _ := res.Outputs["uuid"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(uuid) {
		t.Fatalf("uuid = %q", uuid)
	}
	if n := res.Outputs["int"].(int); n < 0 || n >= 10 {
		t.Fatalf("int %d out of [0,10)", n)
	}
	if len(res.Outputs["hex"].(string)) != 32 {
		t.Fatalf("hex length = %d", len(res.Outputs["hex"].(string)))
	}
}

func TestNow(t *testing.T) {
	res := invoke(t, "now", nil)
	if res.Outputs["unix"].(int64) <= 0 {
		t.Fatal("now.unix should be positive")
	}
	if _, err := time.Parse(time.RFC3339, res.Outputs["iso"].(string)); err != nil {
		t.Fatalf("now.iso not RFC3339: %v", err)
	}
}

func TestSleepIsCappedAndBrief(t *testing.T) {
	start := time.Now()
	res := invoke(t, "sleep", map[string]any{"seconds": "50ms"})
	if time.Since(start) > time.Second {
		t.Fatal("sleep 50ms took too long")
	}
	if res.Outputs["slept_seconds"].(float64) <= 0 {
		t.Fatal("slept_seconds should be > 0")
	}
}

func TestUnknownVerb(t *testing.T) {
	if _, err := (testPlugin{}).Invoke(plugin.InvokeRequest{Verb: "nope"}); err == nil {
		t.Fatal("unknown verb should error")
	}
}

func TestTickEvent(t *testing.T) {
	got := tickEvent(3, "b", "hello")
	ctx := got["context"].(map[string]any)
	if got["event"] != "tick" || got["title"] != "tick 3" {
		t.Fatalf("tick shape = %#v", got)
	}
	if ctx["n"] != 3 || ctx["label"] != "b" || ctx["message"] != "hello" || ctx["labels"] != "b" {
		t.Fatalf("tick context = %#v", ctx)
	}
}

func TestStartSourceEmitsTicks(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := testPlugin{}.StartSource(ctx, plugin.StartSourceRequest{
		Instance: "t",
		Config:   map[string]any{"interval": "50ms", "count": 3, "labels": []any{"a", "b"}},
	}, func(p any) error {
		mu.Lock()
		got = append(got, p.(map[string]any))
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 ticks (count), got %d", len(got))
	}
	// n increments 1,2,3 and labels rotate a,b,a
	wantLabels := []string{"a", "b", "a"}
	for i, ev := range got {
		c := ev["context"].(map[string]any)
		if c["n"] != i+1 {
			t.Fatalf("tick %d has n=%v", i, c["n"])
		}
		if c["label"] != wantLabels[i] {
			t.Fatalf("tick %d label=%v, want %s", i, c["label"], wantLabels[i])
		}
	}
}
