package main

import (
	"context"
	"encoding/json"
	"strings"
)

// rcloneObjects stores objects at <remote>/<set>/<name> with the rclone
// CLI: any of rclone's 70+ backends (S3-compatible, SFTP, WebDAV, SMB, B2,
// Drive, a crypt remote, ...). remote is an rclone path: a named remote
// ("b2:bucket/conductor", configured via env RCLONE_CONFIG_<NAME>_* or a
// config file) or an on-the-fly one (":s3,provider=Other,endpoint=...:bucket").
type rcloneObjects struct {
	remote string
	env    []string
	ssh    *sshCfg // for sftp remotes: rclone speaks SSH itself
}

func (r rcloneObjects) path(set, name string) string {
	p := strings.TrimRight(r.remote, "/") + "/" + set
	if name != "" {
		p += "/" + name
	}
	return p
}

func (r rcloneObjects) rc(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	env := append([]string(nil), r.env...)
	if r.ssh != nil {
		_, keyPath, khPath, cleanup, err := r.ssh.setup()
		if err != nil {
			return nil, err
		}
		defer cleanup()
		if keyPath != "" {
			env = append(env, "RCLONE_SFTP_KEY_FILE="+keyPath)
		}
		if khPath != "" {
			env = append(env, "RCLONE_SFTP_KNOWN_HOSTS_FILE="+khPath)
			// rclone negotiates the server's preferred host key type; Go's
			// knownhosts check then fails if that TYPE isn't listed, even
			// when another listed key is valid. Offer only the listed types.
			if algs := hostKeyAlgs(r.ssh.KnownHosts); algs != "" {
				env = append(env, "RCLONE_SFTP_HOST_KEY_ALGORITHMS="+algs)
			}
		}
		if r.ssh.Port != "" {
			env = append(env, "RCLONE_SFTP_PORT="+r.ssh.Port)
		}
		if proxied() {
			env = append(env, "RCLONE_SFTP_HTTP_PROXY="+proxyEnvURL())
		}
	}
	return run(ctx, env, stdin, "rclone", append([]string{"--config", "", "-q"}, args...)...)
}

func (r rcloneObjects) write(ctx context.Context, set, name string, data []byte) error {
	// rcat streams stdin to one object; most backends make that atomic
	// (S3 multipart, SFTP upload-then-rename via --sftp-... defaults).
	_, err := r.rc(ctx, data, "rcat", "--size", itoa(len(data)), r.path(set, name))
	return err
}

func (r rcloneObjects) read(ctx context.Context, set, name string) ([]byte, error) {
	return r.rc(ctx, nil, "cat", r.path(set, name))
}

func (r rcloneObjects) names(ctx context.Context, set string) ([]string, error) {
	out, err := r.rc(ctx, nil, "lsjson", "--files-only", "--no-modtime", "--no-mimetype", r.path(set, ""))
	if err != nil {
		// A set that has never been written is an empty listing, not an error.
		if te, ok := err.(*toolError); ok && te.code == 3 {
			return nil, nil
		}
		return nil, err
	}
	var items []struct {
		Name string `json:"Name"`
	}
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, err
	}
	ns := make([]string, 0, len(items))
	for _, it := range items {
		ns = append(ns, it.Name)
	}
	return ns, nil
}

func (r rcloneObjects) del(ctx context.Context, set, name string) error {
	_, err := r.rc(ctx, nil, "deletefile", r.path(set, name))
	if te, ok := err.(*toolError); ok && te.code == 4 { // already gone
		return nil
	}
	return err
}

// hostKeyAlgs lists the key types in known_hosts lines, in order, deduped.
func hostKeyAlgs(knownHosts string) string {
	var out []string
	seen := map[string]bool{}
	for _, l := range strings.Split(knownHosts, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || strings.HasPrefix(f[0], "#") {
			continue
		}
		i := 1
		if strings.HasPrefix(f[0], "@") { // @cert-authority / @revoked marker
			i = 2
		}
		if i < len(f) && !seen[f[i]] {
			seen[f[i]] = true
			out = append(out, f[i])
		}
	}
	return strings.Join(out, " ")
}
