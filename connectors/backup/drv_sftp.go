package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sftpObjects stores objects at <dir>/<set>/<name> on an SSH server, over
// a native SSH+SFTP client: no ssh, sftp or rclone binary needed. The
// connection goes through conductor's egress proxy under the jail
// (plugin.DialContext).
type sftpObjects struct {
	addr string // host:port
	user string
	dir  string
	ssh  *sshCfg
}

type sftpConn struct {
	ssh  *ssh.Client
	sftp *sftp.Client
}

func (c *sftpConn) Close() { c.sftp.Close(); c.ssh.Close() }

func (s sftpObjects) connect(ctx context.Context) (*sftpConn, error) {
	if s.ssh == nil || s.ssh.Key == "" {
		return nil, fmt.Errorf("sftp: ssh.key is required")
	}
	signer, err := ssh.ParsePrivateKey([]byte(s.ssh.Key))
	if err != nil {
		return nil, fmt.Errorf("sftp: ssh.key: %w", err)
	}
	cb, algs, err := hostKeyCallback(s.ssh)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{User: s.user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: cb, HostKeyAlgorithms: algs, Timeout: 30 * time.Second}
	dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := plugin.DialContext(dctx, "tcp", s.addr)
	if err != nil {
		return nil, fmt.Errorf("sftp: dial %s: %w", s.addr, err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(raw, s.addr, cfg)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("sftp: ssh handshake with %s: %w", s.addr, err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	sc, err := sftp.NewClient(client)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("sftp: %w", err)
	}
	return &sftpConn{ssh: client, sftp: sc}, nil
}

// hostKeyCallback verifies against known_hosts content (OpenSSH format,
// incl. [host]:port entries), offering only the key types listed so the
// server can't pick an unlisted type and fail the check.
func hostKeyCallback(c *sshCfg) (ssh.HostKeyCallback, []string, error) {
	if c.InsecureAccept {
		return ssh.InsecureIgnoreHostKey(), nil, nil
	}
	if strings.TrimSpace(c.KnownHosts) == "" {
		return nil, nil, fmt.Errorf("ssh: set known_hosts, or insecure_accept_host_key: true")
	}
	f, err := os.CreateTemp("", "kh-")
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(f.Name())
	_, _ = f.WriteString(c.KnownHosts + "\n")
	f.Close()
	cb, err := knownhosts.New(f.Name())
	if err != nil {
		return nil, nil, fmt.Errorf("ssh.known_hosts: %w", err)
	}
	var algs []string
	if a := hostKeyAlgs(c.KnownHosts); a != "" {
		for _, t := range strings.Fields(a) {
			algs = append(algs, t)
			if t == ssh.KeyAlgoRSA { // modern servers sign RSA host keys with SHA-2
				algs = append(algs, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
			}
		}
	}
	return cb, algs, nil
}

func (s sftpObjects) p(set, name string) string { return path.Join(s.dir, set, name) }

func (s sftpObjects) with(ctx context.Context, fn func(*sftp.Client) error) error {
	c, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(c.sftp)
}

func (s sftpObjects) write(ctx context.Context, set, name string, data []byte) error {
	return s.with(ctx, func(c *sftp.Client) error {
		d := s.p(set, "")
		if err := c.MkdirAll(d); err != nil {
			return fmt.Errorf("sftp mkdir %s: %w", d, err)
		}
		tmp := s.p(set, ".tmp-"+name)
		f, err := c.Create(tmp)
		if err != nil {
			return fmt.Errorf("sftp create: %w", err)
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			_ = c.Remove(tmp)
			return err
		}
		if err := f.Close(); err != nil {
			_ = c.Remove(tmp)
			return err
		}
		dst := s.p(set, name)
		// PosixRename replaces atomically where the server supports it.
		if err := c.PosixRename(tmp, dst); err != nil {
			_ = c.Remove(dst)
			if err := c.Rename(tmp, dst); err != nil {
				_ = c.Remove(tmp)
				return fmt.Errorf("sftp rename: %w", err)
			}
		}
		return nil
	})
}

func (s sftpObjects) read(ctx context.Context, set, name string) (out []byte, err error) {
	err = s.with(ctx, func(c *sftp.Client) error {
		f, err := c.Open(s.p(set, name))
		if err != nil {
			return err
		}
		defer f.Close()
		out, err = io.ReadAll(f)
		return err
	})
	return out, err
}

func (s sftpObjects) names(ctx context.Context, set string) (out []string, err error) {
	err = s.with(ctx, func(c *sftp.Client) error {
		ents, err := c.ReadDir(s.p(set, ""))
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		for _, e := range ents {
			if e.Mode().IsRegular() {
				out = append(out, e.Name())
			}
		}
		return nil
	})
	return out, err
}

func (s sftpObjects) del(ctx context.Context, set, name string) error {
	return s.with(ctx, func(c *sftp.Client) error {
		if err := c.Remove(s.p(set, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}

// parseSFTPTarget reads "user@host[:port]:/dir" or sftp://user@host[:port]/dir.
func parseSFTPTarget(target, port string) (addr, user, dir string, err error) {
	if rest, ok := strings.CutPrefix(target, "sftp://"); ok {
		hostpart, p, _ := strings.Cut(rest, "/")
		user, host, ok := strings.Cut(hostpart, "@")
		if !ok {
			return "", "", "", fmt.Errorf("target %q: needs user@host", target)
		}
		if h, pt, e := net.SplitHostPort(host); e == nil {
			host, port = h, pt
		}
		return net.JoinHostPort(host, strOr(port, "22")), user, "/" + p, nil
	}
	user, rest, ok := strings.Cut(target, "@")
	if !ok {
		return "", "", "", fmt.Errorf("target %q: use user@host:/path or sftp://user@host[:port]/path", target)
	}
	host, dir, ok := strings.Cut(rest, ":")
	if !ok || dir == "" {
		return "", "", "", fmt.Errorf("target %q: use user@host:/path", target)
	}
	return net.JoinHostPort(host, strOr(port, "22")), user, dir, nil
}
