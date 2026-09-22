// Command conductor-aws-sqs is the Amazon SQS connector as an external conductor
// plugin. Its headline feature is a SOURCE: it long-polls a queue and emits one
// event per message (deleting it after hand-off, by default). It also exposes
// send_message / receive_message / delete_message / get_queue_attributes verbs.
//
// SDK-free and CLI-free: every request is a plain HTTPS POST to the SQS JSON
// endpoint (application/x-amz-json-1.0, X-Amz-Target: AmazonSQS.<Op>), signed
// with hand-rolled AWS SigV4 from internal/awskit — no aws-sdk-go, no aws CLI.
//
// Connection (used for both Invoke and StartSource):
//
//	region: us-east-1                                   # required
//	queue_url: https://sqs.us-east-1.amazonaws.com/123/my-queue   # default queue (required for the source)
//	access_key_id: ...      # or AWS_ACCESS_KEY_ID env
//	secret_access_key: ...  # or AWS_SECRET_ACCESS_KEY env
//	session_token: ...      # or AWS_SESSION_TOKEN env (for temporary/role creds)
//	endpoint: ...           # override the endpoint (LocalStack, VPC endpoint, tests)
//	wait_time: 20           # long-poll seconds for the source (0..20, default 20)
//	max_messages: 10        # messages per receive for the source (1..10, default 10)
//	visibility_timeout: 30  # seconds a received message is hidden
//	auto_delete: true       # delete a message after it is emitted (default true)
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"

	"github.com/NodeSpy/conductor-plugins/internal/awskit"
)

// nowFunc returns the SigV4 signing time; a var so tests can pin it.
var nowFunc = time.Now

type sqsPlugin struct{}

func (sqsPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "aws-sqs",
		Desc: "Amazon SQS: long-poll a queue as a source (one event per message, auto-deleted after hand-off) plus send/receive/delete/get-attributes verbs. Hand-rolled SigV4 — no AWS SDK, no AWS CLI.",
		Connection: plugin.Schema{
			"region":             {Type: "string", Required: true, Desc: "AWS region, e.g. us-east-1"},
			"queue_url":          {Type: "string", Desc: "default queue URL (required for the source; per-verb queue_url overrides it)"},
			"access_key_id":      {Type: "string", Desc: "AWS access key id (or the AWS_ACCESS_KEY_ID env var)"},
			"secret_access_key":  {Type: "string", Desc: "AWS secret access key (or AWS_SECRET_ACCESS_KEY)"},
			"session_token":      {Type: "string", Desc: "AWS session token for temporary/role credentials (or AWS_SESSION_TOKEN)"},
			"endpoint":           {Type: "string", Desc: "override the SQS endpoint (LocalStack, a VPC endpoint, tests)"},
			"wait_time":          {Type: "integer", Desc: "source long-poll seconds, 0..20 (default 20)"},
			"max_messages":       {Type: "integer", Desc: "source messages per receive, 1..10 (default 10)"},
			"visibility_timeout": {Type: "integer", Desc: "seconds a received message stays hidden"},
			"auto_delete":        {Type: "boolean", Desc: "delete a message after it is emitted by the source (default true)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "send_message", Desc: "send a message to a queue",
				Options: plugin.Schema{
					"queue_url":     {Type: "string", Scope: "queue", Desc: "queue URL (default: the connection's queue_url)"},
					"body":          {Type: "string", Required: true, Desc: "the message body"},
					"delay_seconds": {Type: "integer", Desc: "delay before the message becomes visible (0..900)"},
					"group_id":      {Type: "string", Desc: "MessageGroupId (FIFO queues)"},
					"dedup_id":      {Type: "string", Desc: "MessageDeduplicationId (FIFO queues)"},
				},
				Outputs: plugin.Schema{"message_id": {Type: "string"}, "md5": {Type: "string", Desc: "MD5 of the body"}},
			},
			{
				Name: "receive_message", Desc: "receive up to N messages (one-shot; the source is the streaming path)",
				Options: plugin.Schema{
					"queue_url":          {Type: "string", Scope: "queue"},
					"max_messages":       {Type: "integer", Desc: "1..10 (default 1)"},
					"wait_time":          {Type: "integer", Desc: "long-poll seconds 0..20 (default 0)"},
					"visibility_timeout": {Type: "integer"},
				},
				Outputs: plugin.Schema{"messages": {Type: "list", Desc: "[{message_id, body, receipt_handle, md5}]"}},
			},
			{
				Name: "delete_message", Desc: "delete a message by its receipt handle (ack)",
				Options: plugin.Schema{
					"queue_url":      {Type: "string", Scope: "queue"},
					"receipt_handle": {Type: "string", Required: true, Desc: "from receive_message / the source event"},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
			{
				Name: "get_queue_attributes", Desc: "read a queue's attributes",
				Options: plugin.Schema{
					"queue_url":  {Type: "string", Scope: "queue"},
					"attributes": {Type: "list", Desc: "attribute names (default [\"All\"])"},
				},
				Outputs: plugin.Schema{"attributes": {Type: "map"}},
			},
		},
		Events: []plugin.Event{
			{
				Name: "message",
				Desc: "a message was received from the queue",
				Context: plugin.Schema{
					"message_id":     {Type: "string"},
					"body":           {Type: "string", Desc: "the message body as text"},
					"receipt_handle": {Type: "string", Desc: "use with delete_message when auto_delete is off"},
					"queue_url":      {Type: "string"},
				},
			},
		},
		// The endpoint is region-derived (or an operator override), so no fixed
		// egress is declared — narrow it per instance with network:. Spawns nothing.
		Capabilities: plugin.Capabilities{Spawns: false},
	}
}

func (sqsPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	conn := req.Connection
	if str(conn["region"]) == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "sqs: region is required")
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	queue := strOr(o["queue_url"], str(conn["queue_url"]))

	switch req.Verb {
	case "send_message":
		if str(o["body"]) == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "send_message: body is required")
		}
		payload := map[string]any{"QueueUrl": queue, "MessageBody": str(o["body"])}
		if n, ok := toInt(o["delay_seconds"]); ok {
			payload["DelaySeconds"] = n
		}
		if g := str(o["group_id"]); g != "" {
			payload["MessageGroupId"] = g
		}
		if d := str(o["dedup_id"]); d != "" {
			payload["MessageDeduplicationId"] = d
		}
		res, err := call(ctx, conn, "SendMessage", payload)
		if err != nil {
			return failVerb(req.Verb, err)
		}
		return plugin.InvokeResult{Outputs: map[string]any{"message_id": str(res["MessageId"]), "md5": str(res["MD5OfMessageBody"])}}, nil

	case "receive_message":
		msgs, _, err := receive(ctx, conn, queue, intOr(o["max_messages"], 1), intOr(o["wait_time"], 0), o["visibility_timeout"])
		if err != nil {
			return failVerb(req.Verb, err)
		}
		out := make([]any, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, map[string]any{"message_id": m.MessageID, "body": m.Body, "receipt_handle": m.ReceiptHandle, "md5": m.MD5OfBody})
		}
		return plugin.InvokeResult{Outputs: map[string]any{"messages": out}}, nil

	case "delete_message":
		if str(o["receipt_handle"]) == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "delete_message: receipt_handle is required")
		}
		if _, err := call(ctx, conn, "DeleteMessage", map[string]any{"QueueUrl": queue, "ReceiptHandle": str(o["receipt_handle"])}); err != nil {
			return failVerb(req.Verb, err)
		}
		return plugin.InvokeResult{Outputs: map[string]any{"ok": true}}, nil

	case "get_queue_attributes":
		attrs := strList(o["attributes"])
		if len(attrs) == 0 {
			attrs = []string{"All"}
		}
		res, err := call(ctx, conn, "GetQueueAttributes", map[string]any{"QueueUrl": queue, "AttributeNames": attrs})
		if err != nil {
			return failVerb(req.Verb, err)
		}
		return plugin.InvokeResult{Outputs: map[string]any{"attributes": res["Attributes"]}}, nil
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "sqs: unknown verb "+req.Verb)
}

func (sqsPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	conn := req.Config
	if str(conn["region"]) == "" {
		return fmt.Errorf("sqs: region is required")
	}
	queue := str(conn["queue_url"])
	if queue == "" {
		return fmt.Errorf("sqs: queue_url is required for the source")
	}
	wait := clamp(intOr(conn["wait_time"], 20), 0, 20)
	maxMsgs := clamp(intOr(conn["max_messages"], 10), 1, 10)
	autoDelete := boolvDefault(conn["auto_delete"], true)

	fmt.Fprintf(os.Stderr, "sqs[%s]: polling %s (wait=%ds max=%d auto_delete=%v)\n", req.Instance, queue, wait, maxMsgs, autoDelete)

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for ctx.Err() == nil {
		msgs, _, err := receive(ctx, conn, queue, maxMsgs, wait, conn["visibility_timeout"])
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "sqs[%s]: receive error: %v\n", req.Instance, err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, m := range msgs {
			if err := emit(messageEvent(queue, m)); err != nil {
				continue // don't delete a message we couldn't hand off; visibility timeout redelivers it
			}
			if autoDelete {
				dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				if _, derr := call(dctx, conn, "DeleteMessage", map[string]any{"QueueUrl": queue, "ReceiptHandle": m.ReceiptHandle}); derr != nil {
					fmt.Fprintf(os.Stderr, "sqs[%s]: delete after emit: %v\n", req.Instance, derr)
				}
				cancel()
			}
		}
	}
	return nil
}

func main() {
	if err := plugin.Serve(sqsPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-aws-sqs: %v\n", err)
		os.Exit(1)
	}
}

// message is one SQS message as returned by the JSON protocol.
type message struct {
	MessageID     string `json:"MessageId"`
	ReceiptHandle string `json:"ReceiptHandle"`
	Body          string `json:"Body"`
	MD5OfBody     string `json:"MD5OfBody"`
}

// messageEvent builds the emit payload for one received message. Pure and
// testable: message in, emit map out.
func messageEvent(queueURL string, m message) map[string]any {
	return map[string]any{
		"event": "message",
		"kind":  "message",
		"title": "sqs: " + queueName(queueURL),
		"dedup": m.MessageID,
		"context": map[string]any{
			"message_id":     m.MessageID,
			"body":           m.Body,
			"receipt_handle": m.ReceiptHandle,
			"queue_url":      queueURL,
		},
	}
}

// receive performs one ReceiveMessage and decodes the messages.
func receive(ctx context.Context, conn map[string]any, queue string, maxMsgs, wait int, visibility any) ([]message, map[string]any, error) {
	if queue == "" {
		return nil, nil, fmt.Errorf("queue_url is required")
	}
	payload := map[string]any{
		"QueueUrl":            queue,
		"MaxNumberOfMessages": clamp(maxMsgs, 1, 10),
		"WaitTimeSeconds":     clamp(wait, 0, 20),
	}
	if v, ok := toInt(visibility); ok {
		payload["VisibilityTimeout"] = v
	}
	res, err := call(ctx, conn, "ReceiveMessage", payload)
	if err != nil {
		return nil, nil, err
	}
	// Re-decode the Messages array into typed structs.
	raw, _ := json.Marshal(res["Messages"])
	var msgs []message
	_ = json.Unmarshal(raw, &msgs)
	return msgs, res, nil
}

// call performs one signed SQS JSON POST for the given operation and returns the
// decoded JSON response. A non-2xx status is turned into an error carrying SQS's
// __type / message.
func call(ctx context.Context, conn map[string]any, target string, payload map[string]any) (map[string]any, error) {
	region := str(conn["region"])
	endpoint := strOr(conn["endpoint"], "https://sqs."+region+".amazonaws.com")
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("bad endpoint %q: %w", endpoint, err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	headers := awskit.Sign(awskit.SignInput{
		Method: http.MethodPost,
		Host:   u.Host,
		Path:   "/",
		Body:   body,
		Headers: map[string]string{
			"content-type": "application/x-amz-json-1.0",
			"x-amz-target": "AmazonSQS." + target,
		},
		Service: "sqs",
		Region:  region,
		Creds:   awskit.Load(conn),
		Time:    nowFunc(),
	})

	reqURL := strings.TrimRight(endpoint, "/") + "/"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("sqs %s: %s: %s", target, resp.Status, sqsError(respBody))
	}
	var out map[string]any
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &out); err != nil {
			return nil, fmt.Errorf("sqs %s: decode response: %w", target, err)
		}
	}
	return out, nil
}

// sqsError pulls the human-readable bits out of an SQS JSON error body.
func sqsError(b []byte) string {
	var e struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
		Msg2    string `json:"Message"`
	}
	if json.Unmarshal(b, &e) == nil {
		msg := e.Message
		if msg == "" {
			msg = e.Msg2
		}
		if e.Type != "" || msg != "" {
			return strings.TrimSpace(e.Type + " " + msg)
		}
	}
	return string(b)
}

func failVerb(verb string, err error) (plugin.InvokeResult, error) {
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, verb+": "+err.Error())
}

// queueName is the last path segment of a queue URL, for a readable event title.
func queueName(queueURL string) string {
	if i := strings.LastIndex(queueURL, "/"); i >= 0 && i < len(queueURL)-1 {
		return queueURL[i+1:]
	}
	return queueURL
}

// --- option helpers ---

func str(v any) string { s, _ := v.(string); return s }

func strOr(v any, d string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return d
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

func intOr(v any, d int) int {
	if n, ok := toInt(v); ok {
		return n
	}
	return d
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func boolvDefault(v any, d bool) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1" || x == "yes"
	}
	return d
}

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
