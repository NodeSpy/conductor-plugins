package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/NodeSpy/conductor/pkg/sourcekit"
)

// --- test fixtures: a throwaway RSA key + self-signed cert, signing helpers ---

func testKeyAndCert(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "sns-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return key, pemBytes
}

func sign(t *testing.T, key *rsa.PrivateKey, version string, msg snsMessage) string {
	t.Helper()
	str := canonicalString(msg)
	switch version {
	case "2":
		sum := sha256.Sum256([]byte(str))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatalf("sign v2: %v", err)
		}
		return base64.StdEncoding.EncodeToString(sig)
	default:
		sum := sha1.Sum([]byte(str))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA1, sum[:])
		if err != nil {
			t.Fatalf("sign v1: %v", err)
		}
		return base64.StdEncoding.EncodeToString(sig)
	}
}

// --- canonical string key order ---

func TestCanonicalStringNotification(t *testing.T) {
	msg := snsMessage{
		Type: "Notification", Message: "m", MessageId: "id1", Subject: "subj",
		Timestamp: "t1", TopicArn: "arn:aws:sns:1",
	}
	want := "Message\nm\nMessageId\nid1\nSubject\nsubj\nTimestamp\nt1\nTopicArn\narn:aws:sns:1\nType\nNotification\n"
	if got := canonicalString(msg); got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestCanonicalStringNotificationNoSubject(t *testing.T) {
	msg := snsMessage{Type: "Notification", Message: "m", MessageId: "id1", Timestamp: "t1", TopicArn: "arn"}
	want := "Message\nm\nMessageId\nid1\nTimestamp\nt1\nTopicArn\narn\nType\nNotification\n"
	if got := canonicalString(msg); got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestCanonicalStringSubscriptionConfirmation(t *testing.T) {
	msg := snsMessage{
		Type: "SubscriptionConfirmation", Message: "m", MessageId: "id1",
		SubscribeURL: "https://sns.us-east-1.amazonaws.com/confirm", Timestamp: "t1",
		Token: "tok", TopicArn: "arn",
	}
	want := "Message\nm\nMessageId\nid1\nSubscribeURL\nhttps://sns.us-east-1.amazonaws.com/confirm\n" +
		"Timestamp\nt1\nToken\ntok\nTopicArn\narn\nType\nSubscriptionConfirmation\n"
	if got := canonicalString(msg); got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

// --- signature verification (direct, in-test key — no network) ---

func TestVerifySignatureWithKeyV1AndV2(t *testing.T) {
	key, _ := testKeyAndCert(t)
	base := snsMessage{
		Type: "Notification", Message: "hello", MessageId: "id1", Subject: "subj",
		Timestamp: "2024-01-01T00:00:00Z", TopicArn: "arn:aws:sns:1",
	}

	for _, version := range []string{"1", "2"} {
		msg := base
		msg.SignatureVersion = version
		msg.Signature = sign(t, key, version, msg)
		if err := verifySignatureWithKey(msg, &key.PublicKey); err != nil {
			t.Fatalf("version %s: expected valid signature, got %v", version, err)
		}
	}
}

func TestVerifySignatureWithKeyTampered(t *testing.T) {
	key, _ := testKeyAndCert(t)
	msg := snsMessage{
		Type: "Notification", Message: "hello", MessageId: "id1",
		Timestamp: "2024-01-01T00:00:00Z", TopicArn: "arn:aws:sns:1", SignatureVersion: "1",
	}
	msg.Signature = sign(t, key, "1", msg)

	tampered := msg
	tampered.Message = "goodbye"
	if err := verifySignatureWithKey(tampered, &key.PublicKey); err == nil {
		t.Fatal("expected tampered message to fail verification")
	}
}

func TestVerifySignatureWithKeyUnsupportedVersion(t *testing.T) {
	msg := snsMessage{Type: "Notification", SignatureVersion: "3", Signature: base64.StdEncoding.EncodeToString([]byte("x"))}
	key, _ := testKeyAndCert(t)
	if err := verifySignatureWithKey(msg, &key.PublicKey); err == nil {
		t.Fatal("expected unsupported SignatureVersion to error")
	}
}

// --- SigningCertURL host allowlist ---

func TestValidateCertURLHost(t *testing.T) {
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"valid amazonaws", "https://sns.us-east-1.amazonaws.com/cert.pem", true},
		{"valid bare amazonaws.com", "https://amazonaws.com/cert.pem", true},
		{"http rejected", "http://sns.us-east-1.amazonaws.com/cert.pem", false},
		{"non-amazonaws host rejected", "https://evil.example.com/cert.pem", false},
		{"127.0.0.1 rejected", "https://127.0.0.1:12345/cert.pem", false},
		{"lookalike suffix rejected", "https://notamazonaws.com.evil.com/cert.pem", false},
		{"empty rejected", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCertURLHost(tc.url)
			if tc.ok && err != nil {
				t.Fatalf("expected ok, got error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

// --- cert fetch + parse (against an httptest.Server; host check tested separately) ---

func TestFetchCert(t *testing.T) {
	_, certPEM := testKeyAndCert(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(certPEM)
	}))
	defer srv.Close()

	pub, err := fetchCert(srv.URL)
	if err != nil {
		t.Fatalf("fetchCert: %v", err)
	}
	if pub == nil {
		t.Fatal("fetchCert: nil public key")
	}
}

func TestCertCacheReusesFetch(t *testing.T) {
	_, certPEM := testKeyAndCert(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(certPEM)
	}))
	defer srv.Close()

	c := newCertCache()
	if _, err := c.get(srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := c.get(srv.URL); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("expected 1 fetch, got %d", hits)
	}
}

// --- verifySignature end to end against a real (allowlisted) cert URL is not
// hermetically testable without a real amazonaws.com host; the pieces above
// (canonical string, crypto verify, host allowlist, cert fetch/parse/cache)
// cover it in isolation. handleSubscriptionConfirmation below stubs `verify`
// to exercise the auto-confirm gate without a network round trip.

// --- notification handling / emit ---

func TestHandleNotificationEmitsAndDedups(t *testing.T) {
	var emitted []map[string]any
	s := &snsSource{
		verifySignature: false,
		emit: func(payload any) error {
			emitted = append(emitted, payload.(map[string]any))
			return nil
		},
		dedup:    sourcekit.NewDedup(64),
		instance: "test",
		verify:   func(snsMessage) error { return nil },
	}
	body := []byte(`{
		"Type": "Notification",
		"MessageId": "msg-1",
		"TopicArn": "arn:aws:sns:us-east-1:1:topic",
		"Subject": "hello",
		"Message": "{\"foo\":\"bar\"}",
		"Timestamp": "2024-01-01T00:00:00Z"
	}`)
	s.handle("Notification", body)
	s.handle("Notification", body) // redelivery: must dedup

	if len(emitted) != 1 {
		t.Fatalf("expected 1 emitted event, got %d", len(emitted))
	}
	got := emitted[0]
	if got["event"] != "notification" || got["kind"] != "notification" {
		t.Fatalf("event/kind: %#v", got)
	}
	if got["title"] != "hello" {
		t.Fatalf("title: %#v", got["title"])
	}
	if got["dedup"] != "msg-1" {
		t.Fatalf("dedup: %#v", got["dedup"])
	}
	ctx, ok := got["context"].(map[string]any)
	if !ok {
		t.Fatalf("context: %#v", got["context"])
	}
	if ctx["topic_arn"] != "arn:aws:sns:us-east-1:1:topic" || ctx["subject"] != "hello" {
		t.Fatalf("context: %#v", ctx)
	}
	if ctx["topic_arns"] != "arn:aws:sns:us-east-1:1:topic" || ctx["subjects"] != "hello" {
		t.Fatalf("filter aliases: %#v", ctx)
	}
	mj, ok := ctx["message_json"].(map[string]any)
	if !ok || mj["foo"] != "bar" {
		t.Fatalf("message_json: %#v", ctx["message_json"])
	}
}

func TestHandleNotificationSkipsOnInvalidSignature(t *testing.T) {
	var emitted int
	s := &snsSource{
		verifySignature: true,
		emit:            func(any) error { emitted++; return nil },
		dedup:           sourcekit.NewDedup(64),
		instance:        "test",
		verify:          func(snsMessage) error { return fmt.Errorf("bad signature") },
	}
	body := []byte(`{"Type":"Notification","MessageId":"m1","Message":"x"}`)
	s.handle("Notification", body)
	if emitted != 0 {
		t.Fatalf("expected no emit on invalid signature, got %d", emitted)
	}
}

func TestHandleUnknownAndUnsubscribeTypesDoNotEmit(t *testing.T) {
	var emitted int
	s := &snsSource{
		emit:     func(any) error { emitted++; return nil },
		dedup:    sourcekit.NewDedup(64),
		instance: "test",
		verify:   func(snsMessage) error { return nil },
	}
	s.handle("UnsubscribeConfirmation", []byte(`{"Type":"UnsubscribeConfirmation","TopicArn":"arn"}`))
	s.handle("SomethingElse", []byte(`{"Type":"SomethingElse"}`))
	if emitted != 0 {
		t.Fatalf("expected no emit, got %d", emitted)
	}
}

// --- auto-confirm gate ---

func TestAutoConfirmGetsSubscribeURLOnValidSignature(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &snsSource{
		autoConfirm:     true,
		verifySignature: true,
		emit:            func(any) error { return nil },
		dedup:           sourcekit.NewDedup(64),
		instance:        "test",
		verify:          func(snsMessage) error { return nil }, // stub: signature verifies
	}
	body := []byte(fmt.Sprintf(`{"Type":"SubscriptionConfirmation","MessageId":"m1","SubscribeURL":%q}`, srv.URL))
	s.handle("SubscriptionConfirmation", body)

	if !hit {
		t.Fatal("expected SubscribeURL to be GETed on a valid signature")
	}
}

func TestAutoConfirmDoesNotGetSubscribeURLOnInvalidSignature(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &snsSource{
		autoConfirm:     true,
		verifySignature: true,
		emit:            func(any) error { return nil },
		dedup:           sourcekit.NewDedup(64),
		instance:        "test",
		verify:          func(snsMessage) error { return fmt.Errorf("bad signature") },
	}
	body := []byte(fmt.Sprintf(`{"Type":"SubscriptionConfirmation","MessageId":"m1","SubscribeURL":%q}`, srv.URL))
	s.handle("SubscriptionConfirmation", body)

	if hit {
		t.Fatal("must not GET SubscribeURL on an invalid signature")
	}
}

func TestAutoConfirmDisabledNeverGets(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
	}))
	defer srv.Close()

	s := &snsSource{
		autoConfirm:     false,
		verifySignature: true,
		emit:            func(any) error { return nil },
		dedup:           sourcekit.NewDedup(64),
		instance:        "test",
		verify:          func(snsMessage) error { return nil },
	}
	body := []byte(fmt.Sprintf(`{"Type":"SubscriptionConfirmation","MessageId":"m1","SubscribeURL":%q}`, srv.URL))
	s.handle("SubscriptionConfirmation", body)
	if hit {
		t.Fatal("auto_confirm disabled must never GET SubscribeURL")
	}
}

// --- the security gate ---

func TestRequireSignatureOrAllowUnsigned(t *testing.T) {
	if err := requireSignatureOrAllowUnsigned("sns", true, false); err != nil {
		t.Fatalf("verify_signature true: expected no error, got %v", err)
	}
	if err := requireSignatureOrAllowUnsigned("sns", false, true); err != nil {
		t.Fatalf("verify_signature false + allow_unsigned true: expected no error, got %v", err)
	}
	if err := requireSignatureOrAllowUnsigned("sns", false, false); err == nil {
		t.Fatal("verify_signature false with no allow_unsigned: expected error")
	}
}

func TestStartSourceRefusesUnsignedWithoutAllowUnsigned(t *testing.T) {
	p := sns{}
	err := p.StartSource(context.Background(), plugin.StartSourceRequest{
		Instance: "test",
		Config: map[string]any{
			"listen":           ":0",
			"verify_signature": false,
		},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected StartSource to refuse verify_signature:false without allow_unsigned")
	}
}

func TestStartSourceRequiresListenOrSmee(t *testing.T) {
	p := sns{}
	err := p.StartSource(context.Background(), plugin.StartSourceRequest{
		Instance: "test",
		Config:   map[string]any{},
	}, func(any) error { return nil })
	if err == nil {
		t.Fatal("expected StartSource to require listen or smee")
	}
}

// --- smee SSE payload parsing ---

func TestParseSmeePayloadNestedBody(t *testing.T) {
	raw := []byte(`{
		"host": "example.com",
		"x-amz-sns-message-type": "Notification",
		"body": {"Type": "Notification", "MessageId": "m1", "Message": "hi"},
		"query": {},
		"timestamp": 1700000000000
	}`)
	msgType, body, ok := parseSmeePayload(raw)
	if !ok {
		t.Fatal("expected ok")
	}
	if msgType != "Notification" {
		t.Fatalf("msgType: %q", msgType)
	}
	var m snsMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.MessageId != "m1" || m.Message != "hi" {
		t.Fatalf("body: %#v", m)
	}
}

func TestParseSmeePayloadStringBody(t *testing.T) {
	raw := []byte(`{
		"X-Amz-Sns-Message-Type": "SubscriptionConfirmation",
		"body": "{\"Type\":\"SubscriptionConfirmation\",\"Token\":\"tok\"}"
	}`)
	msgType, body, ok := parseSmeePayload(raw)
	if !ok {
		t.Fatal("expected ok")
	}
	if msgType != "SubscriptionConfirmation" {
		t.Fatalf("msgType: %q", msgType)
	}
	var m snsMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m.Token != "tok" {
		t.Fatalf("body: %#v", m)
	}
}

func TestParseSmeePayloadFallsBackToBodyType(t *testing.T) {
	raw := []byte(`{"body": {"Type": "Notification", "MessageId": "m2"}}`)
	msgType, _, ok := parseSmeePayload(raw)
	if !ok {
		t.Fatal("expected ok")
	}
	if msgType != "Notification" {
		t.Fatalf("msgType: %q", msgType)
	}
}

func TestParseSmeePayloadIgnoresKeepAlive(t *testing.T) {
	raw := []byte(`{"ready": true}`)
	if _, _, ok := parseSmeePayload(raw); ok {
		t.Fatal("expected keep-alive payload with no body to be ignored")
	}
}

func TestParseSmeePayloadFlowsThroughHandle(t *testing.T) {
	var emitted int
	s := &snsSource{
		emit:     func(any) error { emitted++; return nil },
		dedup:    sourcekit.NewDedup(64),
		instance: "test",
		verify:   func(snsMessage) error { return nil },
	}
	raw := []byte(`{
		"x-amz-sns-message-type": "Notification",
		"body": {"Type": "Notification", "MessageId": "m3", "Message": "hi", "Subject": "s"}
	}`)
	msgType, body, ok := parseSmeePayload(raw)
	if !ok {
		t.Fatal("expected ok")
	}
	s.handle(msgType, body)
	if emitted != 1 {
		t.Fatalf("expected 1 emit via smee payload -> handle, got %d", emitted)
	}
}

// --- Describe ---

func TestDescribe(t *testing.T) {
	d := sns{}.Describe()
	if d.Kind != plugin.KindConnector || d.Type != "sns" {
		t.Fatalf("kind/type: %v %q", d.Kind, d.Type)
	}
	wantEgress := []string{"smee.io:443", "sns.*.amazonaws.com:443", "*.amazonaws.com:443"}
	for _, w := range wantEgress {
		if !containsStr(d.Capabilities.Egress, w) {
			t.Errorf("Capabilities.Egress missing %q: %#v", w, d.Capabilities.Egress)
		}
	}
	if d.Capabilities.Spawns {
		t.Errorf("Capabilities.Spawns: want false")
	}
	if len(d.Events) != 1 || d.Events[0].Name != "notification" {
		t.Fatalf("Events: %#v", d.Events)
	}
	ev := d.Events[0]
	for _, k := range []string{"topic_arns", "subjects", "topic_arn", "subject"} {
		if _, ok := ev.Filters[k]; !ok {
			t.Errorf("Filters missing %q", k)
		}
	}
	for _, k := range []string{"topic_arn", "subject", "message", "message_id", "timestamp", "message_json"} {
		if _, ok := ev.Context[k]; !ok {
			t.Errorf("Context missing %q", k)
		}
	}
}

// --- test helpers ---

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
