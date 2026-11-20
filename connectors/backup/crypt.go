package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
)

// age encryption for the file tiers (local, rclone). Writing needs only the
// public recipients; reading (read, verify restore, and an encrypted
// manifest) needs the identity, so a host that only writes backups can be
// given just the public key.

func parseRecipients(rs []string) ([]age.Recipient, error) {
	if len(rs) == 0 {
		return nil, nil
	}
	out, err := age.ParseRecipients(strings.NewReader(strings.Join(rs, "\n")))
	if err != nil {
		return nil, fmt.Errorf("age_recipients: %w", err)
	}
	return out, nil
}

func ageEncrypt(rs []age.Recipient, data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, rs...)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ageDecrypt(identity string, data []byte) ([]byte, error) {
	if strings.TrimSpace(identity) == "" {
		return nil, fmt.Errorf("this snapshot is age-encrypted: set the tier's age_identity to read it")
	}
	ids, err := age.ParseIdentities(strings.NewReader(identity))
	if err != nil {
		return nil, fmt.Errorf("age_identity: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(data), ids...)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return io.ReadAll(r)
}
