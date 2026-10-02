package ghsource

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	secret := "s3cr3t"
	body := []byte(`{"action":"opened"}`)
	good := sign(secret, body)

	if !verifySignature(secret, body, good) {
		t.Fatal("valid signature rejected")
	}
	if verifySignature(secret, body, sign("wrong", body)) {
		t.Fatal("wrong-secret signature accepted")
	}
	if verifySignature(secret, []byte(`{"action":"closed"}`), good) {
		t.Fatal("tampered body accepted")
	}
	if verifySignature(secret, body, "sha1=deadbeef") {
		t.Fatal("non-sha256 prefix accepted")
	}
	if verifySignature(secret, body, "") {
		t.Fatal("empty header accepted")
	}
}
