// Package awskit is a tiny, SDK-free AWS Signature Version 4 (SigV4) signer for
// this repo's AWS connectors — no aws-sdk-go, no aws CLI, stdlib crypto only.
// A connector builds a request, calls Sign to get the headers (including
// Authorization), and sends it over ordinary net/http.
//
// Every function here is pure (the caller supplies the timestamp), so the
// signer is unit-tested directly against the AWS documented "get-vanilla" SigV4
// test vector — see sigv4_test.go.
package awskit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Credentials are the AWS access keys used to sign a request. SessionToken is
// optional (set for temporary/role credentials; it adds X-Amz-Security-Token).
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Load resolves credentials from a connector's config map first
// (access_key_id / secret_access_key / session_token), then falls back to the
// standard AWS_* environment variables — so a role's exported env credentials
// (ECS/Lambda/CI) work without config. It does NOT reach the EC2 instance
// metadata service or SSO; export the credentials or set them in config for
// those. `region` is read separately by the caller.
func Load(cfg map[string]any) Credentials {
	c := Credentials{
		AccessKeyID:     str(cfg["access_key_id"]),
		SecretAccessKey: str(cfg["secret_access_key"]),
		SessionToken:    str(cfg["session_token"]),
	}
	if c.AccessKeyID == "" {
		c.AccessKeyID = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if c.SecretAccessKey == "" {
		c.SecretAccessKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	if c.SessionToken == "" {
		c.SessionToken = os.Getenv("AWS_SESSION_TOKEN")
	}
	return c
}

func str(v any) string { s, _ := v.(string); return s }

// SignInput is everything Sign needs to sign one request.
type SignInput struct {
	Method  string
	Host    string     // the request Host header (endpoint host[:port])
	Path    string     // RAW (unescaped) path, e.g. "/"
	Query   url.Values // may be nil
	Body    []byte
	Headers map[string]string // EXTRA headers to include in the signature (e.g. content-type, x-amz-target)
	Service string            // e.g. "sqs", "ses", "sns"
	Region  string
	Creds   Credentials
	Time    time.Time
}

// Sign computes the AWS SigV4 signature for one request and returns the full set
// of headers (lowercase names) the caller must set, including Authorization.
// host, x-amz-date and x-amz-content-sha256 are added automatically, plus
// x-amz-security-token when the credentials carry a session token; any Headers
// the caller passed are merged in and included in the signature.
func Sign(in SignInput) map[string]string {
	amzDate := in.Time.UTC().Format("20060102T150405Z")
	dateStamp := in.Time.UTC().Format("20060102")
	payloadHash := sha256Hex(in.Body)

	escapedPath := awsURIEncode(in.Path, false)
	canonicalURI := awsURIEncode(escapedPath, false)
	canonicalQueryString := canonicalQuery(in.Query)

	headers := map[string]string{
		"host":                 in.Host,
		"x-amz-date":           amzDate,
		"x-amz-content-sha256": payloadHash,
	}
	if in.Creds.SessionToken != "" {
		headers["x-amz-security-token"] = in.Creds.SessionToken
	}
	for k, v := range in.Headers {
		headers[strings.ToLower(strings.TrimSpace(k))] = v
	}

	canonicalHeadersStr, signedHeadersStr := canonicalHeaders(headers)
	cr := canonicalRequestString(in.Method, canonicalURI, canonicalQueryString, canonicalHeadersStr, signedHeadersStr, payloadHash)
	crHash := sha256Hex([]byte(cr))

	credentialScope := dateStamp + "/" + in.Region + "/" + in.Service + "/aws4_request"
	sts := stringToSign(amzDate, credentialScope, crHash)

	signingKey := deriveSigningKey(in.Creds.SecretAccessKey, dateStamp, in.Region, in.Service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(sts)))

	headers["authorization"] = fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		in.Creds.AccessKeyID, credentialScope, signedHeadersStr, signature)
	return headers
}

func canonicalRequestString(method, canonicalURI, canonicalQueryString, canonicalHeadersStr, signedHeadersStr, payloadHash string) string {
	return strings.Join([]string{
		method, canonicalURI, canonicalQueryString, canonicalHeadersStr, signedHeadersStr, payloadHash,
	}, "\n")
}

func stringToSign(amzDate, credentialScope, hashedCanonicalRequest string) string {
	return strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, credentialScope, hashedCanonicalRequest,
	}, "\n")
}

// deriveSigningKey computes the SigV4 key-derivation chain:
//
//	kDate    = HMAC-SHA256("AWS4" + secret, date)
//	kRegion  = HMAC-SHA256(kDate, region)
//	kService = HMAC-SHA256(kRegion, service)
//	kSigning = HMAC-SHA256(kService, "aws4_request")
func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// canonicalHeaders builds the CanonicalHeaders block and SignedHeaders list:
// names lowercased and sorted, values trimmed, each header line ending in "\n".
func canonicalHeaders(headers map[string]string) (canonicalHeadersStr, signedHeadersStr string) {
	keys := make([]string, 0, len(headers))
	normalized := make(map[string]string, len(headers))
	for k, v := range headers {
		lk := strings.ToLower(strings.TrimSpace(k))
		normalized[lk] = strings.TrimSpace(v)
		keys = append(keys, lk)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(':')
		b.WriteString(normalized[k])
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(keys, ";")
}

// canonicalQuery builds the SigV4 canonical query string: each name and value
// URI-encoded individually (including '/'), sorted by key then value.
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(q))
	for _, k := range keys {
		vals := append([]string{}, q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsURIEncode(k, true)+"="+awsURIEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// awsURIEncode implements AWS's exact URI-encoding rule (stricter than
// net/url): every byte is percent-encoded except the unreserved set
// A-Z a-z 0-9 - _ . ~, uppercase hex. When encodeSlash is false, '/' is left
// unescaped (path segments only).
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreservedByte(c) || (c == '/' && !encodeSlash) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func isUnreservedByte(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '.' || c == '~'
}
