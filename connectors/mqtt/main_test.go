package main

import (
	"reflect"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

func TestDescribe(t *testing.T) {
	d := mqttPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "mqtt" {
		t.Fatalf("kind/type = %v/%v", d.Kind, d.Type)
	}
	if d.Connection["broker"].Required != true {
		t.Fatal("broker should be required")
	}
	if _, ok := d.Connection["subscribe"]; !ok {
		t.Fatal("connection should declare subscribe")
	}
	if len(d.Verbs) != 1 || d.Verbs[0].Name != "publish" {
		t.Fatalf("want one publish verb, got %+v", d.Verbs)
	}
	if d.Verbs[0].Options["topic"].Scope != "topic" {
		t.Fatal("publish.topic should be scoped 'topic'")
	}
	if len(d.Events) != 1 || d.Events[0].Name != "message" {
		t.Fatalf("want one message event, got %+v", d.Events)
	}
	// A message event must publish topic/payload as both context and filter.
	if _, ok := d.Events[0].Context["payload"]; !ok {
		t.Fatal("message event should publish payload context")
	}
	if _, ok := d.Events[0].Filters["topics"]; !ok {
		t.Fatal("message event should filter on topics")
	}
	if d.Capabilities.Spawns {
		t.Fatal("mqtt spawns nothing")
	}
}

func TestMessageEvent(t *testing.T) {
	got := messageEvent("zigbee/kitchen/state", `{"on":true}`, 1, true)
	want := map[string]any{
		"event": "message",
		"kind":  "message",
		"title": "mqtt: zigbee/kitchen/state",
		"context": map[string]any{
			"topic":    "zigbee/kitchen/state",
			"payload":  `{"on":true}`,
			"qos":      1,
			"retained": true,
			"topics":   "zigbee/kitchen/state",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messageEvent = %#v, want %#v", got, want)
	}
}

func TestQoS(t *testing.T) {
	// per-call option wins over the connection default
	if q := qosOf(2, map[string]any{"qos": 1}); q != 2 {
		t.Fatalf("option qos should win, got %d", q)
	}
	// falls back to the connection default
	if q := qosOf(nil, map[string]any{"qos": 1}); q != 1 {
		t.Fatalf("connection qos should apply, got %d", q)
	}
	// out-of-range clamps to 0
	if q := qosOf(9, nil); q != 0 {
		t.Fatalf("invalid qos should clamp to 0, got %d", q)
	}
	// float (JSON numbers decode to float64) is accepted
	if q := qosOf(float64(2), nil); q != 2 {
		t.Fatalf("float qos should work, got %d", q)
	}
}

func TestPublishRequiresTopic(t *testing.T) {
	_, err := mqttPlugin{}.Invoke(plugin.InvokeRequest{
		Verb:    "publish",
		Options: map[string]any{"payload": "hi"},
	})
	if err == nil {
		t.Fatal("publish without topic should error")
	}
}

func TestUnknownVerb(t *testing.T) {
	_, err := mqttPlugin{}.Invoke(plugin.InvokeRequest{Verb: "nope"})
	if err == nil {
		t.Fatal("unknown verb should error")
	}
}

func TestStartSourceRequiresSubscribe(t *testing.T) {
	err := mqttPlugin{}.StartSource(nil, plugin.StartSourceRequest{
		Instance: "t",
		Config:   map[string]any{"broker": "tcp://localhost:1883"},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("StartSource with no subscribe topics should error")
	}
}
