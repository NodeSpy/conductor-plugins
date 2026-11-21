package main

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"filippo.io/age"
)

var (
	setRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	extRe  = regexp.MustCompile(`^[A-Za-z0-9]{1,16}(\.[A-Za-z0-9]{1,16})?$`)
	tierRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$`)
)

// tier is one configured destination.
type tier struct {
	name   string
	driver string
	keep   retention
	store  store
	file   *fileTier // set for local/rclone: needed to read sha/size metadata
	local  bool      // data never leaves this machine

	remoteHost string // host:port a native driver (sftp, s3) connects to
}

// conf is a parsed connection.
type conf struct {
	tiers      []*tier
	allowPaths []string
	allowHosts []string
}

func (c *conf) tier(name string) (*tier, error) {
	if name == "" {
		return c.tiers[0], nil
	}
	for _, t := range c.tiers {
		if t.name == name {
			return t, nil
		}
	}
	return nil, fmt.Errorf("no tier %q (have %s)", name, strings.Join(c.names(), ", "))
}

func (c *conf) names() []string {
	out := make([]string, len(c.tiers))
	for i, t := range c.tiers {
		out[i] = t.name
	}
	return out
}

// problem is one config error at a path.
type problem struct{ path, msg string }

func (p problem) Error() string { return p.path + ": " + p.msg }

// parseConf builds the tiers. It returns every problem it finds, so
// validate can report them all; Invoke fails on the first.
func parseConf(cfg map[string]any) (*conf, []problem) {
	var probs []problem
	add := func(path, f string, a ...any) { probs = append(probs, problem{path, fmt.Sprintf(f, a...)}) }

	c := &conf{allowPaths: strList(cfg["allow_paths"]), allowHosts: strList(cfg["allow_hosts"])}
	base, err := policyFrom(retention{7, 2, 6}, cfg)
	if err != nil {
		add("keep", "%v", err)
	}

	raw, _ := cfg["tiers"].([]any)
	if len(raw) == 0 && str(cfg["dir"]) != "" {
		// Shorthand: `dir:` alone is one local tier.
		raw = []any{map[string]any{"name": "local", "driver": "local", "dir": cfg["dir"]}}
	}
	if len(raw) == 0 {
		add("tiers", "at least one tier is required (or the `dir:` shorthand for a single local tier)")
		return c, probs
	}
	seen := map[string]bool{}
	for i, r := range raw {
		m, _ := r.(map[string]any)
		p := fmt.Sprintf("tiers[%d]", i)
		if m == nil {
			add(p, "must be a map")
			continue
		}
		t := &tier{name: str(m["name"]), driver: str(m["driver"])}
		if t.name == "" {
			t.name = t.driver
		}
		if !tierRe.MatchString(t.name) {
			add(p+".name", "%q: letters, digits, _ or -", t.name)
		} else if seen[t.name] {
			add(p+".name", "duplicate tier name %q", t.name)
		}
		seen[t.name] = true
		if t.keep, err = policyFrom(base, m); err != nil {
			add(p, "%v", err)
		}
		env := envList(m["env"])
		var sc *sshCfg
		if sm, ok := m["ssh"].(map[string]any); ok {
			sc = &sshCfg{Key: str(sm["key"]), KnownHosts: str(sm["known_hosts"]), Port: intStr(sm["port"]), InsecureAccept: boolv(sm["insecure_accept_host_key"])}
		}
		encryptSet, encrypt := m["encrypt"] != nil, boolv(m["encrypt"])
		recips := strList(m["age_recipients"])
		identity := str(m["age_identity"])
		var ars []age.Recipient
		if len(recips) > 0 {
			if ars, err = parseRecipients(recips); err != nil {
				add(p+".age_recipients", "%v", err)
			}
		}
		if identity != "" && len(recips) == 0 {
			// Writing needs the public half; derive it from the identity.
			if id, err := age.ParseX25519Identity(strings.TrimSpace(identity)); err == nil {
				ars = []age.Recipient{id.Recipient()}
			} else {
				add(p+".age_identity", "%v", err)
			}
		}
		autoInit := m["init"] == nil || boolv(m["init"])

		switch t.driver {
		case "local":
			dir := str(m["dir"])
			if !filepath.IsAbs(dir) {
				add(p+".dir", "an absolute path is required")
			}
			if encryptSet && encrypt && len(ars) == 0 {
				add(p+".age_recipients", "encrypt: true needs age_recipients (or age_identity)")
			}
			t.local = true
			t.file = &fileTier{objects: localObjects{dir: filepath.Clean(dir)}, recipients: ars, identity: identity}
			t.store = t.file
		case "rclone":
			remote := str(m["remote"])
			if remote == "" {
				add(p+".remote", "required (an rclone path, e.g. b2:bucket/conductor)")
			}
			// Off-machine data is encrypted unless the operator opts out.
			if len(ars) == 0 && !(encryptSet && !encrypt) {
				add(p+".age_recipients", "an rclone tier leaves this machine: set age_recipients (or point remote at an rclone crypt remote and set encrypt: false)")
			}
			t.file = &fileTier{objects: rcloneObjects{remote: remote, env: env, ssh: sc}, recipients: ars, identity: identity}
			t.store = t.file
		case "sftp":
			target := str(m["target"])
			if sc == nil {
				add(p+".ssh", "required: {key, known_hosts | insecure_accept_host_key}")
				sc = &sshCfg{}
			} else if sc.Key == "" {
				add(p+".ssh.key", "required (a private key; keep it in the vault)")
			}
			addr, user, dir, err := parseSFTPTarget(target, sc.Port)
			if err != nil {
				add(p+".target", "%v", err)
			}
			if len(ars) == 0 && !(encryptSet && !encrypt) {
				add(p+".age_recipients", "an sftp tier leaves this machine: set age_recipients (or encrypt: false)")
			}
			t.remoteHost = addr
			t.file = &fileTier{objects: sftpObjects{addr: addr, user: user, dir: dir, ssh: sc}, recipients: ars, identity: identity}
			t.store = t.file
		case "s3":
			bucket, endpoint, region := str(m["bucket"]), str(m["endpoint"]), str(m["region"])
			key, secret := str(m["access_key_id"]), str(m["secret_access_key"])
			pathStyle := m["path_style"] == nil && endpoint != "" || boolv(m["path_style"])
			if bucket == "" {
				add(p+".bucket", "required")
			}
			if key == "" || secret == "" {
				add(p+".access_key_id", "access_key_id and secret_access_key are required (keep them in the vault)")
			}
			if endpoint != "" {
				if u, err := url.Parse(endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					add(p+".endpoint", "an http(s):// URL, e.g. https://s3.example.com")
				}
			}
			if len(ars) == 0 && !(encryptSet && !encrypt) {
				add(p+".age_recipients", "an s3 tier leaves this machine: set age_recipients (or encrypt: false, e.g. with server-side encryption you trust)")
			}
			t.remoteHost = s3Host(endpoint, region, bucket, pathStyle)
			t.file = &fileTier{objects: newS3(endpoint, region, bucket, str(m["prefix"]), key, secret, pathStyle, m["create_bucket"] == nil || boolv(m["create_bucket"])), recipients: ars, identity: identity}
			t.store = t.file
		case "restic":
			repo, pw := str(m["repo"]), str(m["password"])
			if repo == "" {
				add(p+".repo", "required (e.g. /srv/restic, sftp:user@host:/backups, s3:https://host/bucket)")
			}
			if pw == "" {
				add(p+".password", "required (the repository password; keep it in the vault)")
			}
			t.local = strings.HasPrefix(repo, "/")
			t.store = &resticStore{repo: repo, password: pw, env: env, ssh: sc, autoInit: autoInit, checkPct: str(m["check_read_data"])}
		case "borg":
			repo, pp := str(m["repo"]), str(m["passphrase"])
			enc := "repokey-blake2"
			if encryptSet && !encrypt {
				enc = "none"
			} else if pp == "" {
				add(p+".passphrase", "required (borg repokey encryption; or encrypt: false)")
			}
			if repo == "" {
				add(p+".repo", "required (a local path or ssh://user@host[:port]/path)")
			}
			t.local = strings.HasPrefix(repo, "/")
			t.store = &borgStore{repo: repo, passphrase: pp, encryption: enc, env: env, ssh: sc, autoInit: autoInit, checkData: boolv(m["check_data"])}
		default:
			add(p+".driver", "%q: one of local, sftp, s3, rclone, restic, borg", t.driver)
			continue
		}
		if t.remoteHost != "" && str(m["repo"]) == "" && str(m["remote"]) == "" && !contains(c.allowHosts, t.remoteHost) {
			add(p, "%s is not in allow_hosts (the plugin's jail lets it reach only those)", t.remoteHost)
		}
		if sc != nil && sc.KnownHosts == "" && !sc.InsecureAccept {
			add(p+".ssh.known_hosts", "required (the server's host key line), or insecure_accept_host_key: true")
		}
		for _, loc := range []string{str(m["repo"]), str(m["remote"])} {
			h := hostOf(t.driver, loc)
			if t.remoteHost != "" {
				h = t.remoteHost
			}
			if h == "" && t.driver == "rclone" {
				h = rcloneSFTPHost(loc)
			}
			if h != "" && sc != nil && sc.Port != "" {
				if host, port, err := net.SplitHostPort(h); err == nil && port == "22" {
					h = net.JoinHostPort(host, sc.Port)
				}
			}
			if h != "" && !contains(c.allowHosts, h) {
				add(p, "%s is not in allow_hosts (the plugin's jail lets it reach only those)", h)
			}
		}
		for _, d := range localPaths(t.driver, m) {
			if !under(d, c.allowPaths) {
				add(p, "path %s is not under any allow_paths entry (the plugin may write only there)", d)
			}
		}
		c.tiers = append(c.tiers, t)
	}
	return c, probs
}

// localPaths are the tier's on-this-machine paths that need a write grant.
func localPaths(driver string, m map[string]any) []string {
	switch driver {
	case "local":
		if d := str(m["dir"]); filepath.IsAbs(d) {
			return []string{filepath.Clean(d)}
		}
	case "restic", "borg":
		if r := str(m["repo"]); strings.HasPrefix(r, "/") {
			return []string{filepath.Clean(r)}
		}
	}
	return nil
}

// rcloneSFTPHost reads host from an on-the-fly ":sftp,host=H,...:" remote.
func rcloneSFTPHost(remote string) string {
	rest, ok := strings.CutPrefix(remote, ":sftp,")
	if !ok {
		return ""
	}
	params, _, _ := strings.Cut(rest, ":")
	host, port := "", "22"
	for _, kv := range strings.Split(params, ",") {
		k, v, _ := strings.Cut(kv, "=")
		v = strings.Trim(v, `'"`)
		switch k {
		case "host":
			host = v
		case "port":
			port = v
		}
	}
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, port)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func under(p string, roots []string) bool {
	for _, r := range roots {
		r = filepath.Clean(r)
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func policyFrom(base retention, m map[string]any) (retention, error) {
	r := base
	for key, dst := range map[string]*int{"keep_daily": &r.daily, "keep_weekly": &r.weekly, "keep_monthly": &r.monthly} {
		if v, ok := m[key]; ok && v != nil {
			n := intv(v)
			if n < 0 {
				return r, fmt.Errorf("%s must be >= 0", key)
			}
			*dst = n
		}
	}
	return r, nil
}

// envList reads an env map {KEY: value} into KEY=value pairs, sorted.
func envList(v any) []string {
	m, _ := v.(map[string]any)
	out := make([]string, 0, len(m))
	for k, val := range m {
		out = append(out, k+"="+fmt.Sprint(val))
	}
	sort.Strings(out)
	return out
}

// hostOf extracts host:port from a repo/remote when it names one directly.
func hostOf(driver, s string) string {
	switch {
	case strings.HasPrefix(s, "ssh://"), strings.HasPrefix(s, "sftp://"):
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		port := u.Port()
		if port == "" {
			port = "22"
		}
		return net.JoinHostPort(u.Hostname(), port)
	case strings.HasPrefix(s, "sftp:"):
		h := sftpHost(s)
		if _, host, ok := strings.Cut(h, "@"); ok {
			h = host
		}
		return net.JoinHostPort(h, "22")
	case strings.HasPrefix(s, "s3:http"), strings.HasPrefix(s, "rest:http"):
		u, err := url.Parse(s[strings.Index(s, ":")+1:])
		if err != nil {
			return ""
		}
		port := u.Port()
		if port == "" {
			port = map[bool]string{true: "443", false: "80"}[u.Scheme == "https"]
		}
		return net.JoinHostPort(u.Hostname(), port)
	}
	return ""
}

// --- value helpers ---

func str(v any) string { s, _ := v.(string); return s }

func strOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func boolv(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1" || x == "yes"
	}
	return false
}

func intv(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func intStr(v any) string {
	if v == nil {
		return ""
	}
	if n := intv(v); n > 0 {
		return strconv.Itoa(n)
	}
	return ""
}

func itoa(n int) string { return strconv.Itoa(n) }

func strList(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case []string:
		return x
	case string:
		if strings.TrimSpace(x) != "" {
			return []string{strings.TrimSpace(x)}
		}
	}
	return nil
}
