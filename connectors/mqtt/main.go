// Command conductor-mqtt is the MQTT connector as an external conductor plugin.
// It exposes one verb — publish — and one source: a live subscription to one or
// more topic filters on an MQTT broker (MQTT 3.1.1 over tcp/ssl/ws/wss), built
// on eclipse/paho.mqtt.golang. The source is a persistent broker connection
// this plugin holds open (paho reconnects it automatically and re-subscribes on
// reconnect), emitting one event per received message — the natural fit for a
// homelab where Home Assistant, sensors, and Zigbee/Z-Wave bridges publish to
// MQTT.
//
// Connection (used for both Invoke and StartSource):
//
//	broker: "tcp://mqtt.local:1883"  # tcp:// | ssl:// | ws:// | wss:// (required)
//	client_id: "conductor"           # MQTT client id (default conductor-mqtt-<instance>)
//	username: "<user>"               # optional broker auth
//	password: "<pass>"
//	insecure_skip_verify: false      # skip TLS verification for ssl:// with a self-signed cert
//	qos: 0                           # 0 | 1 | 2 — subscribe/publish QoS (default 0)
//	subscribe: ["home/#", "zigbee/+/state"]  # topic filters (StartSource only; + and # wildcards)
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type mqttPlugin struct{}

func (mqttPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "mqtt",
		Desc: "MQTT: publish a message to a topic (verb), and subscribe to topic filters as a live source — one event per received message.",
		Connection: plugin.Schema{
			"broker":               {Type: "string", Required: true, Desc: "broker URL: tcp:// | ssl:// | ws:// | wss:// (a bare host:port is treated as tcp://)"},
			"client_id":            {Type: "string", Desc: "MQTT client id (default conductor-mqtt-<instance>)"},
			"username":             {Type: "string", Desc: "broker username (optional)"},
			"password":             {Type: "string", Desc: "broker password (optional)"},
			"insecure_skip_verify": {Type: "boolean", Desc: "skip TLS certificate verification for ssl:// / wss:// (self-signed brokers)"},
			"qos":                  {Type: "integer", Desc: "MQTT QoS 0|1|2 for subscribe and publish (default 0)"},
			"subscribe":            {Type: "list", Desc: "topic filters to subscribe to (StartSource only); + (one level) and # (rest) wildcards"},
		},
		Verbs: []plugin.Verb{
			{
				Name:  "publish",
				Desc:  "publish a message to a topic",
				Usage: "publish a payload to an MQTT topic; topic is required",
				Options: plugin.Schema{
					"topic":   {Type: "string", Required: true, Scope: "topic", Desc: "the topic to publish to (no wildcards)"},
					"payload": {Type: "string", Desc: "message payload (alias: message)"},
					"message": {Type: "string", Desc: "message payload (alias of payload)"},
					"qos":     {Type: "integer", Desc: "QoS 0|1|2 for this message (default: the connection's qos)"},
					"retain":  {Type: "boolean", Desc: "set the retained flag so the broker keeps this as the topic's last value"},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
		},
		Events: []plugin.Event{
			{
				Name: "message",
				Desc: "a message was received on a subscribed topic filter",
				Context: plugin.Schema{
					"topic":    {Type: "string", Desc: "the concrete topic the message arrived on"},
					"payload":  {Type: "string", Desc: "the message payload as text"},
					"qos":      {Type: "integer"},
					"retained": {Type: "boolean", Desc: "true if this was the broker's retained last value, not a live publish"},
				},
				Filters: plugin.Schema{
					"topics": {Type: "list", Desc: "match any of these exact topics"},
					"topic":  {Type: "string", Desc: "match this exact topic"},
				},
			},
		},
		// The broker host is operator-specific (config), so no fixed egress is
		// declared — narrow it per instance with network:. Spawns nothing.
		Capabilities: plugin.Capabilities{Spawns: false},
	}
}

func (mqttPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	if req.Verb != "publish" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "mqtt: unknown verb "+req.Verb)
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	topic := str(o["topic"])
	if topic == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "publish: topic is required")
	}
	payload := str(o["payload"])
	if payload == "" {
		payload = str(o["message"])
	}
	qos := qosOf(o["qos"], req.Connection)
	retain := boolv(o["retain"])

	client, err := connect(req.Connection, "")
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "publish: connect: "+err.Error())
	}
	defer client.Disconnect(250)

	tok := client.Publish(topic, qos, retain, payload)
	if !tok.WaitTimeout(15*time.Second) || tok.Error() != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "publish: "+tokErr(tok))
	}
	return plugin.InvokeResult{Outputs: map[string]any{"ok": true}}, nil
}

func (mqttPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	topics := strList(cfg["subscribe"])
	if len(topics) == 0 {
		return fmt.Errorf("mqtt: subscribe must list at least one topic filter")
	}
	qos := qosOf(nil, cfg)

	// The message handler runs on paho's own goroutine(s); emit is safe to call
	// concurrently (the SDK serializes writes), so we emit straight from it.
	onMessage := func(_ mqtt.Client, m mqtt.Message) {
		_ = emit(messageEvent(m.Topic(), string(m.Payload()), int(m.Qos()), m.Retained()))
	}
	// Subscribe from OnConnect so the subscriptions are re-established every time
	// paho reconnects, not just on the first connect.
	filters := make(map[string]byte, len(topics))
	for _, t := range topics {
		filters[t] = qos
	}
	onConnect := func(c mqtt.Client) {
		if tok := c.SubscribeMultiple(filters, onMessage); tok.WaitTimeout(15*time.Second) && tok.Error() != nil {
			fmt.Fprintf(os.Stderr, "mqtt[%s]: subscribe: %v\n", req.Instance, tok.Error())
		}
	}

	clientID := strOr(cfg["client_id"], "conductor-mqtt-"+req.Instance)
	client, err := connectWith(cfg, clientID, onConnect)
	if err != nil {
		return fmt.Errorf("mqtt: connect: %w", err)
	}
	fmt.Fprintf(os.Stderr, "mqtt[%s]: subscribed to %s\n", req.Instance, strings.Join(topics, ", "))

	<-ctx.Done()
	client.Disconnect(250)
	return nil
}

func main() {
	if err := plugin.Serve(mqttPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-mqtt: %v\n", err)
		os.Exit(1)
	}
}

// messageEvent builds the emit payload for one received MQTT message: the event
// name plus the context the daemon filters and templates on. It is the pure,
// testable core of the source — topic/payload in, emit map out. `topics` is a
// singular-topic alias so the documented filters: {topics: [...]} list-contains
// vocabulary matches the daemon's generic filter evaluator.
func messageEvent(topic, payload string, qos int, retained bool) map[string]any {
	return map[string]any{
		"event": "message",
		"kind":  "message",
		"title": "mqtt: " + topic,
		"context": map[string]any{
			"topic":    topic,
			"payload":  payload,
			"qos":      qos,
			"retained": retained,
			"topics":   topic, // singular alias for list-contains filtering
		},
	}
}

// connect opens a paho client with no onConnect handler (the verb path, which
// publishes once and disconnects).
func connect(cfg map[string]any, clientID string) (mqtt.Client, error) {
	return connectWith(cfg, clientID, nil)
}

// connectWith builds and connects a paho client from the connection map. A nil
// onConnect is fine (the publish path); the source passes one so it re-subscribes
// on every reconnect.
func connectWith(cfg map[string]any, clientID string, onConnect mqtt.OnConnectHandler) (mqtt.Client, error) {
	broker := str(cfg["broker"])
	if broker == "" {
		return nil, fmt.Errorf("broker is required")
	}
	if !strings.Contains(broker, "://") {
		broker = "tcp://" + broker
	}
	opts := mqtt.NewClientOptions()
	opts.AddBroker(broker)
	if clientID == "" {
		clientID = "conductor-mqtt"
	}
	opts.SetClientID(clientID)
	if u := str(cfg["username"]); u != "" {
		opts.SetUsername(u)
		opts.SetPassword(str(cfg["password"]))
	}
	opts.SetCleanSession(true)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectTimeout(15 * time.Second)
	opts.SetMaxReconnectInterval(30 * time.Second)
	if boolv(cfg["insecure_skip_verify"]) {
		opts.SetTLSConfig(&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // opt-in, documented, for self-signed homelab brokers
	}
	if onConnect != nil {
		opts.SetOnConnectHandler(onConnect)
	}

	client := mqtt.NewClient(opts)
	tok := client.Connect()
	if !tok.WaitTimeout(20*time.Second) || tok.Error() != nil {
		return nil, fmt.Errorf("%s", tokErr(tok))
	}
	return client, nil
}

// qosOf resolves the QoS: the per-call option if present and valid, else the
// connection's qos, clamped to 0..2.
func qosOf(opt any, cfg map[string]any) byte {
	if q, ok := toInt(opt); ok {
		return clampQoS(q)
	}
	if q, ok := toInt(cfg["qos"]); ok {
		return clampQoS(q)
	}
	return 0
}

func clampQoS(q int) byte {
	if q < 0 || q > 2 {
		return 0
	}
	return byte(q)
}

func tokErr(tok mqtt.Token) string {
	if tok.Error() != nil {
		return tok.Error().Error()
	}
	return "timed out"
}

// --- option helpers (mirror the other connectors) ---

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
}

func boolv(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1" || x == "yes"
	}
	return false
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

// strList accepts a string, []string, or []any and returns a non-empty slice.
func strList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
