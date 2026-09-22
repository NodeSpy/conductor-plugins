package awskit

import (
	"net/http"
	"os"
	"testing"
	"time"
)

// SigV4 tests against the AWS documented "get-vanilla" test vector
// (aws-sig-v4-test-suite): fixed credentials AKIDEXAMPLE /
// wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY, us-east-1/service, a GET / at
// 20150830T123600Z. The canonical request, string-to-sign, and signature below
// are the suite's own .creq/.sts/.authz values — not derived from this code.
const (
	vectorAccessKey = "AKIDEXAMPLE"
	vectorSecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorRegion    = "us-east-1"
	vectorService   = "service"
	vectorHost      = "example.amazonaws.com"

	vectorStringToSign = "AWS4-HMAC-SHA256\n" +
		"20150830T123600Z\n" +
		"20150830/us-east-1/service/aws4_request\n" +
		"bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63"

	vectorSignature = "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"

	vectorEmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func TestSignatureMatchesVector(t *testing.T) {
	// deriveSigningKey + HMAC against the suite's published string-to-sign must
	// reproduce the suite's published signature exactly.
	key := deriveSigningKey(vectorSecretKey, "20150830", vectorRegion, vectorService)
	sig := hexEncode(hmacSHA256(key, []byte(vectorStringToSign)))
	if sig != vectorSignature {
		t.Fatalf("signature: got %s want %s", sig, vectorSignature)
	}
}

func TestCanonicalRequestMatchesVector(t *testing.T) {
	headers := map[string]string{"host": vectorHost, "x-amz-date": "20150830T123600Z"}
	ch, sh := canonicalHeaders(headers)
	if sh != "host;x-amz-date" {
		t.Fatalf("signed headers: %q", sh)
	}
	cr := canonicalRequestString(http.MethodGet, "/", "", ch, sh, vectorEmptyPayloadHash)
	want := "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + vectorEmptyPayloadHash
	if cr != want {
		t.Fatalf("canonical request:\n got:  %q\n want: %q", cr, want)
	}
}

// TestSignEndToEnd checks the whole Sign function is internally consistent.
// Sign always adds x-amz-content-sha256 to the signed set (which the two-header
// vector does not), so it recomputes the expected Authorization from Sign's own
// three-header set rather than asserting byte-equality with vectorSignature
// (already verified independently above).
func TestSignEndToEnd(t *testing.T) {
	when, _ := time.Parse("20060102T150405Z", "20150830T123600Z")
	got := Sign(SignInput{
		Method:  http.MethodGet,
		Host:    vectorHost,
		Path:    "/",
		Service: vectorService,
		Region:  vectorRegion,
		Creds:   Credentials{AccessKeyID: vectorAccessKey, SecretAccessKey: vectorSecretKey},
		Time:    when,
	})
	if got["x-amz-content-sha256"] != vectorEmptyPayloadHash {
		t.Fatalf("content hash: %q", got["x-amz-content-sha256"])
	}
	headers := map[string]string{
		"host":                 vectorHost,
		"x-amz-date":           "20150830T123600Z",
		"x-amz-content-sha256": vectorEmptyPayloadHash,
	}
	ch, sh := canonicalHeaders(headers)
	if sh != "host;x-amz-content-sha256;x-amz-date" {
		t.Fatalf("signed headers: %q", sh)
	}
	cr := canonicalRequestString(http.MethodGet, "/", "", ch, sh, vectorEmptyPayloadHash)
	sts := stringToSign("20150830T123600Z", "20150830/us-east-1/service/aws4_request", sha256Hex([]byte(cr)))
	key := deriveSigningKey(vectorSecretKey, "20150830", vectorRegion, vectorService)
	wantSig := hexEncode(hmacSHA256(key, []byte(sts)))
	want := "AWS4-HMAC-SHA256 Credential=" + vectorAccessKey + "/20150830/us-east-1/service/aws4_request, SignedHeaders=" + sh + ", Signature=" + wantSig
	if got["authorization"] != want {
		t.Fatalf("authorization:\n got:  %q\n want: %q", got["authorization"], want)
	}
}

func TestSignSignsExtraHeadersAndSessionToken(t *testing.T) {
	when, _ := time.Parse("20060102T150405Z", "20150830T123600Z")
	got := Sign(SignInput{
		Method:  http.MethodPost,
		Host:    "sqs.us-east-1.amazonaws.com",
		Path:    "/",
		Service: "sqs",
		Region:  "us-east-1",
		Headers: map[string]string{"content-type": "application/x-amz-json-1.0", "x-amz-target": "AmazonSQS.SendMessage"},
		Creds:   Credentials{AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: "TOKEN"},
		Time:    when,
	})
	// the extra headers and the session token must appear in SignedHeaders
	auth := got["authorization"]
	for _, want := range []string{"content-type", "x-amz-security-token", "x-amz-target"} {
		if !containsSignedHeader(auth, want) {
			t.Fatalf("SignedHeaders should include %q; auth=%q", want, auth)
		}
	}
	if got["x-amz-security-token"] != "TOKEN" {
		t.Fatal("session token header not set")
	}
}

func TestLoadFallsBackToEnv(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "envAK")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "envSK")
	os.Unsetenv("AWS_SESSION_TOKEN")
	// config wins when present
	if c := Load(map[string]any{"access_key_id": "cfgAK", "secret_access_key": "cfgSK"}); c.AccessKeyID != "cfgAK" {
		t.Fatalf("config should win, got %q", c.AccessKeyID)
	}
	// env fills the gaps
	if c := Load(map[string]any{}); c.AccessKeyID != "envAK" || c.SecretAccessKey != "envSK" {
		t.Fatalf("env fallback failed: %+v", c)
	}
}

func containsSignedHeader(auth, h string) bool {
	// SignedHeaders=...; the header list is ';'-joined between "SignedHeaders=" and ", Signature="
	i := indexOf(auth, "SignedHeaders=")
	if i < 0 {
		return false
	}
	rest := auth[i+len("SignedHeaders="):]
	j := indexOf(rest, ", Signature=")
	if j >= 0 {
		rest = rest[:j]
	}
	for _, p := range splitSemis(rest) {
		if p == h {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func splitSemis(s string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == ';' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(s[i])
		}
	}
	return append(out, cur)
}

func hexEncode(b []byte) string {
	const hextable = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hextable[c>>4]
		out[i*2+1] = hextable[c&0x0f]
	}
	return string(out)
}
