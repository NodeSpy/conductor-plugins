package rpctest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"testing"
	"time"
)

// FreeAddr reserves and releases a localhost port, returning the address for a
// plugin's webhook listener to bind.
func FreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// HMACHex is the hex-encoded HMAC-SHA256 of body under secret — the shape
// Sentry, GitHub, and PagerDuty webhook signatures all wrap.
func HMACHex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// PostUntilAccepted retries a POST until the plugin's listener is up and
// answers 202 Accepted, then returns. It fails the test if that never happens
// (the listener never bound, or the signature was rejected).
func PostUntilAccepted(t *testing.T, url string, headers map[string]string, body []byte) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		r, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(r)
		if err == nil {
			last = resp.StatusCode
			resp.Body.Close()
			if resp.StatusCode == http.StatusAccepted {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("never delivered a webhook the plugin accepted to %s (last status %d)", url, last)
}

// PostStatus performs one POST and returns the status code — for asserting a
// bad-signature delivery is refused.
func PostStatus(t *testing.T, url string, headers map[string]string, body []byte) int {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
