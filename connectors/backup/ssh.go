package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

// sshCfg is a tier's `ssh:` block, shared by restic (sftp), borg and rclone
// (sftp). The key and known_hosts are given as CONTENT (from the vault), and
// written to 0600 files in the per-call staging dir for the call's lifetime.
type sshCfg struct {
	Key            string
	KnownHosts     string
	Port           string
	InsecureAccept bool
}

// sshSetup materializes the key/known_hosts files and returns the ssh
// command words (for BORG_RSH / restic sftp.command) and a cleanup func.
func (c *sshCfg) setup() (words []string, keyPath, khPath string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "backup-ssh-")
	if err != nil {
		return nil, "", "", func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	words = []string{"ssh", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "ServerAliveInterval=30"}
	if c.Key != "" {
		keyPath = filepath.Join(dir, "key")
		k := c.Key
		if !strings.HasSuffix(k, "\n") {
			k += "\n"
		}
		if err = os.WriteFile(keyPath, []byte(k), 0o600); err != nil {
			cleanup()
			return nil, "", "", func() {}, err
		}
		words = append(words, "-i", keyPath)
	}
	switch {
	case c.InsecureAccept:
		words = append(words, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR")
	case c.KnownHosts != "":
		khPath = filepath.Join(dir, "known_hosts")
		if err = os.WriteFile(khPath, []byte(c.KnownHosts+"\n"), 0o600); err != nil {
			cleanup()
			return nil, "", "", func() {}, err
		}
		words = append(words, "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+khPath)
	default:
		cleanup()
		return nil, "", "", func() {}, fmt.Errorf("ssh: set known_hosts (the server's host key line), or insecure_accept_host_key: true")
	}
	if c.Port != "" {
		words = append(words, "-p", c.Port)
	}
	// Under the plugin jail the only way out is conductor's egress proxy; ssh
	// reaches it through this same binary acting as a CONNECT tunnel.
	if proxied() {
		if self, e := os.Executable(); e == nil {
			words = append(words, "-o", "ProxyCommand="+shellQuote(self)+" "+connectArg+" %h %p")
		}
	}
	return words, keyPath, khPath, cleanup, nil
}

func proxied() bool {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

func proxyEnvURL() string {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

const connectArg = "__connect"

// connectMain is the ProxyCommand mode: pipe stdin/stdout to host:port
// through the egress proxy (plugin.DialContext).
func connectMain(host, port string) int {
	c, err := plugin.DialContext(bgCtx(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "conductor-backup connect:", err)
		return 1
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(c, os.Stdin); done <- struct{}{} }()
	go func() { _, _ = io.Copy(os.Stdout, c); done <- struct{}{} }()
	<-done
	_ = c.Close()
	return 0
}
