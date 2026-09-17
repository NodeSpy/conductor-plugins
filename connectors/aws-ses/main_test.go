package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// --- SigV4 pure-function tests against the AWS documented "get-vanilla"
// SigV4 test vector (aws-sig-v4-test-suite / get-vanilla), the standard
// fixed test credentials AKIDEXAMPLE / wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY,
// region us-east-1, service "service", date 2015-08-30T12:36:00Z, GET / with
// only Host and X-Amz-Date signed and an empty body. Expected canonical
// request, string-to-sign, and signature are taken verbatim from the test
// suite's .creq/.sts/.authz files — not derived from this implementation.

const (
	vectorAccessKey = "AKIDEXAMPLE"
	vectorSecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorRegion    = "us-east-1"
	vectorService   = "service"
	vectorHost      = "example.amazonaws.com"

	// get-vanilla.creq
	vectorCanonicalRequest = "GET\n" +
		"/\n" +
		"\n" +
		"host:example.amazonaws.com\n" +
		"x-amz-date:20150830T123600Z\n" +
		"\n" +
		"host;x-amz-date\n" +
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// get-vanilla.sts
	vectorStringToSign = "AWS4-HMAC-SHA256\n" +
		"20150830T123600Z\n" +
		"20150830/us-east-1/service/aws4_request\n" +
		"bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63"

	// get-vanilla.authz (the Signature= value)
	vectorSignature = "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"

	vectorEmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func vectorTime(t *testing.T) time.Time {
	t.Helper()
	ts, err := time.Parse("20060102T150405Z", "20150830T123600Z")
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestSigV4EmptyPayloadHash(t *testing.T) {
	if got := sha256Hex(nil); got != vectorEmptyPayloadHash {
		t.Errorf("sha256Hex(nil): got %s want %s", got, vectorEmptyPayloadHash)
	}
	if got := sha256Hex([]byte("")); got != vectorEmptyPayloadHash {
		t.Errorf("sha256Hex(\"\"): got %s want %s", got, vectorEmptyPayloadHash)
	}
}

func TestSigV4CanonicalRequest(t *testing.T) {
	headers := map[string]string{
		"host":       vectorHost,
		"x-amz-date": "20150830T123600Z",
	}
	canonicalHeadersStr, signedHeadersStr := canonicalHeaders(headers)
	if want := "host;x-amz-date"; signedHeadersStr != want {
		t.Fatalf("signedHeadersStr: got %q want %q", signedHeadersStr, want)
	}
	escapedPath := awsURIEncode("/", false)
	canonicalURI := awsURIEncode(escapedPath, false)
	if canonicalURI != "/" {
		t.Fatalf("canonicalURI: got %q want %q", canonicalURI, "/")
	}
	cr := canonicalRequestString(http.MethodGet, canonicalURI, "", canonicalHeadersStr, signedHeadersStr, vectorEmptyPayloadHash)
	if cr != vectorCanonicalRequest {
		t.Fatalf("canonical request:\n got:  %q\n want: %q", cr, vectorCanonicalRequest)
	}
}

func TestSigV4StringToSign(t *testing.T) {
	crHash := sha256Hex([]byte(vectorCanonicalRequest))
	credentialScope := "20150830/us-east-1/service/aws4_request"
	sts := stringToSign("20150830T123600Z", credentialScope, crHash)
	if sts != vectorStringToSign {
		t.Fatalf("string to sign:\n got:  %q\n want: %q", sts, vectorStringToSign)
	}
}

func TestSigV4Signature(t *testing.T) {
	signingKey := deriveSigningKey(vectorSecretKey, "20150830", vectorRegion, vectorService)
	sig := hexEncode(hmacSHA256(signingKey, []byte(vectorStringToSign)))
	if sig != vectorSignature {
		t.Fatalf("signature: got %s want %s", sig, vectorSignature)
	}
}

// hexEncode is a tiny local encoder so this test reads like the spec's
// "signature = hex(HMAC(kSigning, string-to-sign))" step, independent of
// main.go's own use of encoding/hex.
func hexEncode(b []byte) string {
	const hextable = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hextable[c>>4]
		out[i*2+1] = hextable[c&0x0f]
	}
	return string(out)
}

// TestSigV4FullVectorPipeline exercises the whole signV4 function end-to-end
// with the get-vanilla vector's exact request (method/host/path/time/
// credentials). signV4 always adds x-amz-content-sha256 to the signed set
// (see package doc / connection doc), which the vector's minimal two-header
// example does not — so this does not assert byte-equality against
// vectorSignature (that exact value is already verified independently, with
// the vector's own host+x-amz-date-only header set, by TestSigV4Signature
// and TestSigV4CanonicalRequest/TestSigV4StringToSign above). Instead it
// confirms the full pipeline is internally consistent: recompute the
// signature by hand from signV4's own building blocks, using the same
// three-header set signV4 actually signs, and check it matches exactly.
func TestSigV4FullVectorPipeline(t *testing.T) {
	out := signV4(sigV4Input{
		Method:          http.MethodGet,
		Host:            vectorHost,
		Path:            "/",
		AccessKeyID:     vectorAccessKey,
		SecretAccessKey: vectorSecretKey,
		Region:          vectorRegion,
		Service:         vectorService,
		Time:            vectorTime(t),
	})
	if got := out.Headers["x-amz-date"]; got != "20150830T123600Z" {
		t.Errorf("x-amz-date: got %q", got)
	}
	if out.EscapedPath != "/" {
		t.Errorf("EscapedPath: got %q", out.EscapedPath)
	}
	payloadHash := vectorEmptyPayloadHash
	if got := out.Headers["x-amz-content-sha256"]; got != payloadHash {
		t.Fatalf("x-amz-content-sha256: got %q want %q", got, payloadHash)
	}

	headers := map[string]string{
		"host":                 vectorHost,
		"x-amz-date":           "20150830T123600Z",
		"x-amz-content-sha256": payloadHash,
	}
	canonicalHeadersStr, signedHeadersStr := canonicalHeaders(headers)
	if want := "host;x-amz-content-sha256;x-amz-date"; signedHeadersStr != want {
		t.Fatalf("signedHeadersStr: got %q want %q", signedHeadersStr, want)
	}
	cr := canonicalRequestString(http.MethodGet, "/", "", canonicalHeadersStr, signedHeadersStr, payloadHash)
	crHash := sha256Hex([]byte(cr))
	credentialScope := "20150830/us-east-1/service/aws4_request"
	sts := stringToSign("20150830T123600Z", credentialScope, crHash)
	signingKey := deriveSigningKey(vectorSecretKey, "20150830", vectorRegion, vectorService)
	wantSig := hexEncode(hmacSHA256(signingKey, []byte(sts)))

	want := "AWS4-HMAC-SHA256 Credential=" + vectorAccessKey + "/" + credentialScope + ", " +
		"SignedHeaders=" + signedHeadersStr + ", Signature=" + wantSig
	if got := out.Headers["authorization"]; got != want {
		t.Fatalf("Authorization:\n got:  %q\n want: %q", got, want)
	}
}

func TestAWSURIEncode(t *testing.T) {
	cases := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"/", false, "/"},
		{"/a/b", false, "/a/b"},
		{"user@example.com", true, "user%40example.com"},
		{"user@example.com", false, "user%40example.com"},
		{"a b", true, "a%20b"},
		{"a/b", true, "a%2Fb"},
	}
	for _, tc := range cases {
		if got := awsURIEncode(tc.in, tc.encodeSlash); got != tc.want {
			t.Errorf("awsURIEncode(%q, %v): got %q want %q", tc.in, tc.encodeSlash, got, tc.want)
		}
	}
}

// --- plugin-level tests (hermetic httptest) ---

func fixedNow(t *testing.T, ts string) func() {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatal(err)
	}
	old := nowFunc
	nowFunc = func() time.Time { return parsed }
	return func() { nowFunc = old }
}

func testConn(srv *httptest.Server) map[string]any {
	return map[string]any{
		"access_key_id":     "AKIATEST",
		"secret_access_key": "test-secret",
		"region":            "us-east-1",
		"endpoint":          srv.URL,
	}
}

func checkSigned(t *testing.T, r *http.Request) {
	t.Helper()
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIATEST/") {
		t.Errorf("Authorization header: got %q", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=") || !strings.Contains(auth, "Signature=") {
		t.Errorf("Authorization header missing SignedHeaders/Signature: %q", auth)
	}
	if r.Header.Get("X-Amz-Date") == "" {
		t.Errorf("X-Amz-Date header missing")
	}
	if r.Header.Get("X-Amz-Content-Sha256") == "" {
		t.Errorf("X-Amz-Content-Sha256 header missing")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestSendEmail(t *testing.T) {
	defer fixedNow(t, "2024-01-15T10:00:00Z")()

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkSigned(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/v2/email/outbound-emails" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"MessageId": "msg-1"})
	}))
	defer srv.Close()

	p := newSESPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "send_email", Connection: testConn(srv),
		Options: map[string]any{
			"from": "[email protected]", "to": []any{"[email protected]"}, "cc": []any{"[email protected]"},
			"subject": "hi", "text": "plain body", "html": "<b>html</b>", "reply_to": []any{"[email protected]"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["MessageId"] != "msg-1" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	if gotBody["FromEmailAddress"] != "[email protected]" {
		t.Errorf("FromEmailAddress: %#v", gotBody["FromEmailAddress"])
	}
	dest, ok := gotBody["Destination"].(map[string]any)
	if !ok {
		t.Fatalf("Destination: %#v", gotBody["Destination"])
	}
	toAddrs, ok := dest["ToAddresses"].([]any)
	if !ok || len(toAddrs) != 1 || toAddrs[0] != "[email protected]" {
		t.Errorf("Destination.ToAddresses: %#v", dest["ToAddresses"])
	}
	ccAddrs, ok := dest["CcAddresses"].([]any)
	if !ok || len(ccAddrs) != 1 || ccAddrs[0] != "[email protected]" {
		t.Errorf("Destination.CcAddresses: %#v", dest["CcAddresses"])
	}
	if _, ok := dest["BccAddresses"]; ok {
		t.Errorf("Destination.BccAddresses should be absent when bcc is unset: %#v", dest["BccAddresses"])
	}
	content, ok := gotBody["Content"].(map[string]any)
	if !ok {
		t.Fatalf("Content: %#v", gotBody["Content"])
	}
	simple, ok := content["Simple"].(map[string]any)
	if !ok {
		t.Fatalf("Content.Simple: %#v", content["Simple"])
	}
	subject, ok := simple["Subject"].(map[string]any)
	if !ok || subject["Data"] != "hi" {
		t.Errorf("Content.Simple.Subject: %#v", simple["Subject"])
	}
	simpleBody, ok := simple["Body"].(map[string]any)
	if !ok {
		t.Fatalf("Content.Simple.Body: %#v", simple["Body"])
	}
	textPart, ok := simpleBody["Text"].(map[string]any)
	if !ok || textPart["Data"] != "plain body" {
		t.Errorf("Content.Simple.Body.Text: %#v", simpleBody["Text"])
	}
	htmlPart, ok := simpleBody["Html"].(map[string]any)
	if !ok || htmlPart["Data"] != "<b>html</b>" {
		t.Errorf("Content.Simple.Body.Html: %#v", simpleBody["Html"])
	}
	replyTo, ok := gotBody["ReplyToAddresses"].([]any)
	if !ok || len(replyTo) != 1 || replyTo[0] != "[email protected]" {
		t.Errorf("ReplyToAddresses: %#v", gotBody["ReplyToAddresses"])
	}
}

func TestSendEmailRequiresTextOrHtml(t *testing.T) {
	p := newSESPlugin()
	conn := map[string]any{"access_key_id": "a", "secret_access_key": "s", "region": "us-east-1", "endpoint": "http://example.invalid"}
	_, err := p.Invoke(plugin.InvokeRequest{
		Verb: "send_email", Connection: conn,
		Options: map[string]any{"from": "[email protected]", "to": []any{"[email protected]"}, "subject": "hi"},
	})
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

func TestSendTemplatedEmail(t *testing.T) {
	defer fixedNow(t, "2024-01-15T10:00:00Z")()

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkSigned(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/v2/email/outbound-emails" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		writeJSON(w, 200, map[string]any{"MessageId": "msg-2"})
	}))
	defer srv.Close()

	p := newSESPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "send_templated_email", Connection: testConn(srv),
		Options: map[string]any{
			"from": "[email protected]", "to": []any{"[email protected]"},
			"template_name": "welcome",
			"template_data": map[string]any{"name": "Dave"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 200 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	content, ok := gotBody["Content"].(map[string]any)
	if !ok {
		t.Fatalf("Content: %#v", gotBody["Content"])
	}
	tmpl, ok := content["Template"].(map[string]any)
	if !ok || tmpl["TemplateName"] != "welcome" {
		t.Fatalf("Content.Template: %#v", content["Template"])
	}
	tdRaw, ok := tmpl["TemplateData"].(string)
	if !ok {
		t.Fatalf("TemplateData not a string: %#v", tmpl["TemplateData"])
	}
	var td map[string]any
	if err := json.Unmarshal([]byte(tdRaw), &td); err != nil {
		t.Fatalf("TemplateData not valid JSON: %v (%q)", err, tdRaw)
	}
	if td["name"] != "Dave" {
		t.Errorf("TemplateData.name: %#v", td["name"])
	}
}

func TestIdentities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkSigned(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/v2/email/identities" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"EmailIdentities": []any{
			map[string]any{"IdentityName": "[email protected]"},
		}})
	}))
	defer srv.Close()

	p := newSESPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "identities", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestIdentityGetAndCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkSigned(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/email/identities/[email protected]":
			writeJSON(w, 200, map[string]any{"VerifiedForSendingStatus": true})
		case r.Method == http.MethodPost && r.URL.Path == "/v2/email/identities":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			if body["EmailIdentity"] != "[email protected]" {
				t.Errorf("create_identity body: %#v", body)
			}
			writeJSON(w, 200, map[string]any{"IdentityType": "EMAIL_ADDRESS"})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSESPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "identity_get", Connection: testConn(srv),
		Options: map[string]any{"email_identity": "[email protected]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok || result["VerifiedForSendingStatus"] != true {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "create_identity", Connection: testConn(srv),
		Options: map[string]any{"email_identity": "[email protected]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok = res.Outputs["result"].(map[string]any)
	if !ok || result["IdentityType"] != "EMAIL_ADDRESS" {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestGetSendQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkSigned(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/v2/email/account" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"SendQuota": map[string]any{"Max24HourSend": 200}})
	}))
	defer srv.Close()

	p := newSESPlugin()
	res, err := p.Invoke(plugin.InvokeRequest{Verb: "get_send_quota", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Outputs["result"].(map[string]any); !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
}

func TestSuppressedListSuppressUnsuppress(t *testing.T) {
	var gotSuppressBody map[string]any
	var gotUnsuppressMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkSigned(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/email/suppression/addresses":
			writeJSON(w, 200, map[string]any{"SuppressedDestinationSummaries": []any{
				map[string]any{"EmailAddress": "[email protected]"},
			}})
		case r.Method == http.MethodPut && r.URL.Path == "/v2/email/suppression/addresses/[email protected]":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotSuppressBody)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/v2/email/suppression/addresses/[email protected]":
			gotUnsuppressMethod = r.Method
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	p := newSESPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{Verb: "suppressed_list", Connection: testConn(srv)})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "suppress", Connection: testConn(srv),
		Options: map[string]any{"email": "[email protected]", "reason": "BOUNCE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outputs["status_code"] != 204 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
	if gotSuppressBody["Reason"] != "BOUNCE" {
		t.Errorf("suppress body: %#v", gotSuppressBody)
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "unsuppress", Connection: testConn(srv),
		Options: map[string]any{"email": "[email protected]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotUnsuppressMethod != http.MethodDelete {
		t.Errorf("unsuppress method: got %q", gotUnsuppressMethod)
	}
	if res.Outputs["status_code"] != 204 {
		t.Errorf("status_code: %#v", res.Outputs["status_code"])
	}
}

func TestAPIEscapeHatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkSigned(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/email/account":
			writeJSON(w, 200, map[string]any{"SendQuota": map[string]any{"Max24HourSend": 50}})
		case r.Method == http.MethodGet && r.URL.Path == "/v2/email/identities" && r.URL.Query().Get("NextToken") == "abc":
			writeJSON(w, 200, []any{map[string]any{"IdentityName": "[email protected]"}})
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()

	p := newSESPlugin()

	res, err := p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"path": "/v2/email/account"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := res.Outputs["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %#v", res.Outputs["result"])
	}
	if _, ok := result["SendQuota"]; !ok {
		t.Errorf("result missing SendQuota: %#v", result)
	}

	res, err = p.Invoke(plugin.InvokeRequest{
		Verb: "api", Connection: testConn(srv),
		Options: map[string]any{"method": "GET", "path": "/v2/email/identities", "query": map[string]any{"NextToken": "abc"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, ok := res.Outputs["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items: %#v", res.Outputs["items"])
	}
}

func TestNon2xxIsInternalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"signature does not match"}`))
	}))
	defer srv.Close()

	p := newSESPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "get_send_quota", Connection: testConn(srv)})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok {
		t.Fatalf("expected *plugin.Error, got %T: %v", err, err)
	}
	if pe.Code != plugin.CodeInternalError {
		t.Errorf("code: got %d want %d", pe.Code, plugin.CodeInternalError)
	}
	if !strings.Contains(pe.Message, "403") || !strings.Contains(pe.Message, "signature does not match") {
		t.Errorf("message should carry status + body, got %q", pe.Message)
	}
}

func TestMissingRequiredConnection(t *testing.T) {
	p := newSESPlugin()
	cases := []map[string]any{
		{"secret_access_key": "s", "region": "us-east-1"},
		{"access_key_id": "a", "region": "us-east-1"},
		{"access_key_id": "a", "secret_access_key": "s"},
	}
	for i, conn := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: "get_send_quota", Connection: conn})
		if err == nil {
			t.Errorf("case %d: expected error for missing connection field", i)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("case %d: expected CodeInvalidParams, got %v", i, err)
		}
	}
}

func TestMissingRequiredOptions(t *testing.T) {
	p := newSESPlugin()
	conn := map[string]any{"access_key_id": "a", "secret_access_key": "s", "region": "us-east-1", "endpoint": "http://example.invalid"}

	cases := []struct {
		verb string
		opts map[string]any
	}{
		{"send_email", map[string]any{}},
		{"send_email", map[string]any{"from": "[email protected]", "to": []any{"[email protected]"}, "subject": "hi"}}, // missing text/html
		{"send_templated_email", map[string]any{}},
		{"identity_get", map[string]any{}},
		{"create_identity", map[string]any{}},
		{"suppress", map[string]any{}},
		{"suppress", map[string]any{"email": "[email protected]"}}, // missing reason
		{"unsuppress", map[string]any{}},
		{"api", map[string]any{}},
	}
	for _, tc := range cases {
		_, err := p.Invoke(plugin.InvokeRequest{Verb: tc.verb, Connection: conn, Options: tc.opts})
		if err == nil {
			t.Errorf("%s: expected error for missing required options", tc.verb)
			continue
		}
		pe, ok := err.(*plugin.Error)
		if !ok || pe.Code != plugin.CodeInvalidParams {
			t.Errorf("%s: expected CodeInvalidParams, got %v", tc.verb, err)
		}
	}
}

func TestUnknownVerb(t *testing.T) {
	p := newSESPlugin()
	conn := map[string]any{"access_key_id": "a", "secret_access_key": "s", "region": "us-east-1"}
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "nope", Connection: conn})
	if err == nil {
		t.Fatal("expected error")
	}
	pe, ok := err.(*plugin.Error)
	if !ok || pe.Code != plugin.CodeInvalidParams {
		t.Fatalf("expected CodeInvalidParams, got %v", err)
	}
}

func TestDefaultEndpoint(t *testing.T) {
	conn, err := parseConn(map[string]any{"access_key_id": "a", "secret_access_key": "s", "region": "eu-west-1"})
	if err != nil {
		t.Fatal(err)
	}
	if conn.endpoint != "https://email.eu-west-1.amazonaws.com" {
		t.Errorf("default endpoint: got %q", conn.endpoint)
	}
	if conn.host != "email.eu-west-1.amazonaws.com" {
		t.Errorf("default host: got %q", conn.host)
	}
}

func TestSessionTokenHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Amz-Security-Token"); got != "stst-token" {
			t.Errorf("X-Amz-Security-Token: got %q", got)
		}
		auth := r.Header.Get("Authorization")
		if !strings.Contains(auth, "x-amz-security-token") {
			t.Errorf("SignedHeaders should include x-amz-security-token: %q", auth)
		}
		writeJSON(w, 200, map[string]any{})
	}))
	defer srv.Close()

	conn := testConn(srv)
	conn["session_token"] = "stst-token"

	p := newSESPlugin()
	_, err := p.Invoke(plugin.InvokeRequest{Verb: "get_send_quota", Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDescribe(t *testing.T) {
	p := newSESPlugin()
	d := p.Describe()
	if d.Kind != plugin.KindConnector {
		t.Errorf("kind: got %v want %v", d.Kind, plugin.KindConnector)
	}
	if d.Type != "aws-ses" {
		t.Errorf("type: got %q", d.Type)
	}
	if len(d.Events) != 0 {
		t.Errorf("expected no events (verb-only), got %v", d.Events)
	}
	for _, key := range []string{"access_key_id", "secret_access_key", "region"} {
		f := d.Connection[key]
		if f.Type != "string" || !f.Required {
			t.Errorf("connection.%s: %#v", key, f)
		}
	}
	for _, key := range []string{"session_token", "endpoint"} {
		f := d.Connection[key]
		if f.Type != "string" || f.Required {
			t.Errorf("connection.%s: %#v", key, f)
		}
	}

	want := []string{
		"send_email", "send_templated_email", "identities", "identity_get",
		"create_identity", "get_send_quota", "suppressed_list", "suppress",
		"unsuppress", "api",
	}
	got := map[string]bool{}
	for _, v := range d.Verbs {
		got[v.Name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("Describe missing verb %q", w)
		}
	}
	if len(d.Verbs) != len(want) {
		t.Errorf("verb count: got %d want %d", len(d.Verbs), len(want))
	}
	if len(d.Capabilities.Egress) != 1 || d.Capabilities.Egress[0] != "email.*.amazonaws.com:443" {
		t.Errorf("egress: got %v", d.Capabilities.Egress)
	}
}
