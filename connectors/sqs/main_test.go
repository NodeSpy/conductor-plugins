package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// mockSQS is an httptest server that routes by the X-Amz-Target header and
// returns canned JSON, so the whole signed-POST call path is exercised without
// real AWS. It records the last request for assertions.
type mockSQS struct {
	*httptest.Server
	lastTarget string
	lastBody   map[string]any
	lastAuth   string
}

func newMockSQS(t *testing.T, responses map[string]any) *mockSQS {
	t.Helper()
	m := &mockSQS{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.lastAuth = r.Header.Get("Authorization")
		target := r.Header.Get("X-Amz-Target")
		m.lastTarget = target
		raw, _ := io.ReadAll(r.Body)
		m.lastBody = map[string]any{}
		_ = json.Unmarshal(raw, &m.lastBody)
		op := strings.TrimPrefix(target, "AmazonSQS.")
		resp, ok := responses[op]
		if !ok {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"__type":"com.amazon.coral.service#UnknownOperation","message":"no canned response"}`))
			return
		}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	}))
	t.Cleanup(m.Close)
	return m
}

func testConn(m *mockSQS) map[string]any {
	return map[string]any{
		"region":            "us-east-1",
		"endpoint":          m.URL,
		"queue_url":         m.URL + "/123456789012/my-queue",
		"access_key_id":     "AKID",
		"secret_access_key": "SECRET",
	}
}

func TestDescribe(t *testing.T) {
	d := sqsPlugin{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "sqs" {
		t.Fatalf("kind/type = %v/%v", d.Kind, d.Type)
	}
	if d.Connection["region"].Required != true {
		t.Fatal("region should be required")
	}
	names := map[string]bool{}
	for _, v := range d.Verbs {
		names[v.Name] = true
	}
	for _, want := range []string{"send_message", "receive_message", "delete_message", "get_queue_attributes"} {
		if !names[want] {
			t.Fatalf("missing verb %q", want)
		}
	}
	if len(d.Events) != 1 || d.Events[0].Name != "message" {
		t.Fatalf("want one message event, got %+v", d.Events)
	}
	if d.Capabilities.Spawns {
		t.Fatal("sqs spawns nothing")
	}
}

func TestSendMessage(t *testing.T) {
	m := newMockSQS(t, map[string]any{"SendMessage": map[string]any{"MessageId": "m-1", "MD5OfMessageBody": "abc"}})
	res, err := sqsPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "send_message", Connection: testConn(m),
		Options: map[string]any{"body": "hello", "delay_seconds": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["message_id"] != "m-1" {
		t.Fatalf("message_id = %v", res.Outputs["message_id"])
	}
	if m.lastTarget != "AmazonSQS.SendMessage" {
		t.Fatalf("target = %q", m.lastTarget)
	}
	if m.lastBody["MessageBody"] != "hello" {
		t.Fatalf("MessageBody = %v", m.lastBody["MessageBody"])
	}
	if !strings.HasPrefix(m.lastAuth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("request was not SigV4-signed: %q", m.lastAuth)
	}
}

func TestSendRequiresBody(t *testing.T) {
	m := newMockSQS(t, map[string]any{})
	if _, err := (sqsPlugin{}).Invoke(plugin.InvokeRequest{Verb: "send_message", Connection: testConn(m), Options: map[string]any{}}); err == nil {
		t.Fatal("send_message without body should error")
	}
}

func TestReceiveMessage(t *testing.T) {
	m := newMockSQS(t, map[string]any{"ReceiveMessage": map[string]any{
		"Messages": []any{
			map[string]any{"MessageId": "m-1", "ReceiptHandle": "rh-1", "Body": "job", "MD5OfBody": "x"},
		},
	}})
	res, err := sqsPlugin{}.Invoke(plugin.InvokeRequest{Verb: "receive_message", Connection: testConn(m), Options: map[string]any{"max_messages": 5}})
	if err != nil {
		t.Fatal(err)
	}
	msgs := res.Outputs["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	got := msgs[0].(map[string]any)
	if got["body"] != "job" || got["receipt_handle"] != "rh-1" {
		t.Fatalf("message = %#v", got)
	}
}

func TestDeleteMessage(t *testing.T) {
	m := newMockSQS(t, map[string]any{"DeleteMessage": map[string]any{}})
	res, err := sqsPlugin{}.Invoke(plugin.InvokeRequest{
		Verb: "delete_message", Connection: testConn(m),
		Options: map[string]any{"receipt_handle": "rh-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["ok"] != true {
		t.Fatal("delete should report ok")
	}
	if m.lastBody["ReceiptHandle"] != "rh-1" {
		t.Fatalf("ReceiptHandle = %v", m.lastBody["ReceiptHandle"])
	}
}

func TestErrorSurfacesSQSType(t *testing.T) {
	// No canned SendMessage response → the mock returns a 400 with an SQS error
	// body, which the connector must surface.
	m := newMockSQS(t, map[string]any{})
	_, err := sqsPlugin{}.Invoke(plugin.InvokeRequest{Verb: "send_message", Connection: testConn(m), Options: map[string]any{"body": "x"}})
	if err == nil {
		t.Fatal("a 400 should surface as an error")
	}
	if !strings.Contains(err.Error(), "UnknownOperation") {
		t.Fatalf("error should carry the SQS __type: %v", err)
	}
}

func TestMissingRegion(t *testing.T) {
	if _, err := (sqsPlugin{}).Invoke(plugin.InvokeRequest{Verb: "send_message", Connection: map[string]any{}, Options: map[string]any{"body": "x"}}); err == nil {
		t.Fatal("missing region should error")
	}
}

func TestMessageEvent(t *testing.T) {
	got := messageEvent("https://sqs.us-east-1.amazonaws.com/123/orders", message{MessageID: "m-1", ReceiptHandle: "rh", Body: "b"})
	want := map[string]any{
		"event": "message",
		"kind":  "message",
		"title": "sqs: orders",
		"dedup": "m-1",
		"context": map[string]any{
			"message_id":     "m-1",
			"body":           "b",
			"receipt_handle": "rh",
			"queue_url":      "https://sqs.us-east-1.amazonaws.com/123/orders",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("messageEvent = %#v, want %#v", got, want)
	}
}

func TestQueueNameAndClamp(t *testing.T) {
	if queueName("https://sqs.us-east-1.amazonaws.com/123/my-queue") != "my-queue" {
		t.Fatal("queueName last segment")
	}
	if clamp(50, 1, 10) != 10 || clamp(0, 1, 10) != 1 || clamp(5, 1, 10) != 5 {
		t.Fatal("clamp")
	}
}
