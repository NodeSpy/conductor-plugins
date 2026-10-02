package ghsource

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"hash"
)

// verifySignature checks the GitHub X-Hub-Signature-256 header against the body
// using the webhook secret. header is like "sha256=abc123…".
func verifySignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return false
	}
	want, err := hex.DecodeString(header[len(prefix):])
	if err != nil {
		return false
	}
	var mac hash.Hash = hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

// VerifySignature reports whether header (X-Hub-Signature-256, "sha256=<hex>")
// is a valid HMAC-SHA256 of body under secret — strict: only the sha256= form
// GitHub sends is accepted, and an empty or malformed header fails.
func VerifySignature(secret string, body []byte, header string) bool {
	return verifySignature(secret, body, header)
}
