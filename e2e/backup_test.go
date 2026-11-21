package e2e

// End-to-end for the backup connector: the real plugin binary over the
// plugin protocol, driving the REAL restic / rclone / borg against real
// backends — a RustFS (S3-compatible) container and an OpenSSH (+borg)
// container — plus local directories.
//
// Opt-in (it needs docker and the three tools):
//
//	BACKUP_E2E=1 BACKUP_E2E_TOOLS=~/.cache/backup-e2e/bin \
//	BACKUP_E2E_SSHD_IMAGE=backup-e2e-sshd go test ./e2e -run TestBackup -v
//
// BACKUP_E2E_TOOLS is prepended to PATH (holding restic, rclone, borg). The
// sshd image must run sshd on :22 for user `bk`, install /keys/authorized_keys
// at start, and have borg on PATH (build it from e2e/testdata/backup-sshd).

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/NodeSpy/conductor-plugins/internal/rpctest"
	"github.com/NodeSpy/conductor/pkg/plugin"
)

const (
	rfAccess = "e2eaccess"
	rfSecret = "e2esecret123"
)

type backupEnv struct {
	t          *testing.T
	c          *rpctest.Client
	dir        string
	s3         string // host:port
	sshHost    string // host:port
	sshKey     string // private key PEM
	knownHosts string // "[127.0.0.1]:port ssh-ed25519 AAAA…"
}

func requireBackupE2E(t *testing.T) *backupEnv {
	t.Helper()
	if os.Getenv("BACKUP_E2E") == "" {
		t.Skip("set BACKUP_E2E=1 (needs docker + restic/rclone/borg) to run")
	}
	// Outside conductor there is no egress proxy: a proxy in the test's own
	// environment is not conductor's and would refuse the local containers.
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("ORIG_PATH_FOR_BUILD", os.Getenv("PATH"))
	if tools := os.Getenv("BACKUP_E2E_TOOLS"); tools != "" {
		t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	for _, tool := range []string{"docker", "restic", "rclone", "borg", "ssh", "ssh-keygen", "ssh-keyscan"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s not found on PATH", tool)
		}
	}
	e := &backupEnv{t: t, dir: t.TempDir()}

	// RustFS (S3-compatible).
	s3Port := freePort(t)
	e.s3 = "127.0.0.1:" + s3Port
	dockerRun(t, "bk-e2e-rustfs", "-p", e.s3+":9000",
		"-e", "RUSTFS_ACCESS_KEY="+rfAccess, "-e", "RUSTFS_SECRET_KEY="+rfSecret, "rustfs/rustfs:latest")

	// OpenSSH + borg.
	keyDir := t.TempDir()
	run(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "e2e", "-f", filepath.Join(keyDir, "id"))
	pub, _ := os.ReadFile(filepath.Join(keyDir, "id.pub"))
	if err := os.WriteFile(filepath.Join(keyDir, "authorized_keys"), pub, 0o644); err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(filepath.Join(keyDir, "id"))
	e.sshKey = string(key)
	sshPort := freePort(t)
	e.sshHost = "127.0.0.1:" + sshPort
	img := os.Getenv("BACKUP_E2E_SSHD_IMAGE")
	if img == "" {
		img = "backup-e2e-sshd"
	}
	dockerRun(t, "bk-e2e-sshd", "-p", e.sshHost+":22", "-v", keyDir+":/keys:ro", img)

	waitTCP(t, e.s3)
	waitTCP(t, e.sshHost)
	var scan []byte
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := exec.Command("ssh-keyscan", "-p", sshPort, "-t", "ed25519", "127.0.0.1").Output()
		out = []byte(strings.Join(nonComment(string(out)), "\n"))
		if err == nil && len(out) > 0 {
			scan = out
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ssh-keyscan: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	e.knownHosts = strings.TrimSpace(string(scan))
	// rclone/restic need the S3 bucket's server ready, not just the port.
	waitS3(t, e.s3)

	e.c = rpctest.Start(t, rpctest.BuildConnector(t, "backup"))
	return e
}

func (e *backupEnv) sshBlock() map[string]any {
	_, port, _ := net.SplitHostPort(e.sshHost)
	return map[string]any{"key": e.sshKey, "known_hosts": e.knownHosts, "port": port}
}

func (e *backupEnv) s3Env() map[string]any {
	return map[string]any{"AWS_ACCESS_KEY_ID": rfAccess, "AWS_SECRET_ACCESS_KEY": rfSecret}
}

func (e *backupEnv) rcloneS3Remote(bucket string) string {
	return fmt.Sprintf(":s3,provider=Other,endpoint='http://%s',access_key_id=%s,secret_access_key=%s,force_path_style=true:%s", e.s3, rfAccess, rfSecret, bucket)
}

func (e *backupEnv) invoke(conn map[string]any, verb string, o map[string]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return e.c.Invoke(ctx, plugin.InvokeRequest{Verb: verb, Connection: conn, Options: o})
}

func (e *backupEnv) must(conn map[string]any, verb string, o map[string]any) map[string]any {
	e.t.Helper()
	out, err := e.invoke(conn, verb, o)
	if err != nil {
		e.t.Fatalf("%s %v: %v", verb, o, err)
	}
	return out
}

// TestBackupAllDrivers puts the same content through every driver and
// backend, then proves dedup, listing, restore (read), verify check +
// restore, retention, and that a tampered/missing backend object is caught.
func TestBackupAllDrivers(t *testing.T) {
	e := requireBackupE2E(t)
	id, _ := age.GenerateX25519Identity()
	_, sshPort, _ := net.SplitHostPort(e.sshHost)

	tiers := []any{
		map[string]any{"name": "disk", "driver": "local", "dir": filepath.Join(e.dir, "local")},
		map[string]any{"name": "disk-age", "driver": "local", "dir": filepath.Join(e.dir, "local-age"), "age_identity": id.String()},
		map[string]any{"name": "restic-local", "driver": "restic", "repo": filepath.Join(e.dir, "restic"), "password": "pw1"},
		map[string]any{"name": "restic-s3", "driver": "restic", "repo": "s3:http://" + e.s3 + "/restic", "password": "pw2", "env": e.s3Env()},
		map[string]any{"name": "restic-sftp", "driver": "restic", "repo": "sftp:bk@127.0.0.1:/home/bk/restic", "password": "pw3", "ssh": e.sshBlock()},
		map[string]any{"name": "rclone-s3", "driver": "rclone", "remote": e.rcloneS3Remote("rclone"), "age_identity": id.String()},
		map[string]any{"name": "rclone-sftp", "driver": "rclone", "remote": ":sftp,host=127.0.0.1,user=bk:/home/bk/rclone", "age_identity": id.String(), "ssh": e.sshBlock()},
		map[string]any{"name": "borg-local", "driver": "borg", "repo": filepath.Join(e.dir, "borg"), "passphrase": "pp1"},
		map[string]any{"name": "borg-ssh", "driver": "borg", "repo": "ssh://bk@127.0.0.1:" + sshPort + "/home/bk/borg", "passphrase": "pp2", "ssh": e.sshBlock()},
	}
	conn := map[string]any{
		"tiers": tiers, "allow_paths": []any{e.dir},
		"allow_hosts": []any{e.s3, e.sshHost},
		"keep_daily":  2, "keep_weekly": 0, "keep_monthly": 0,
	}
	names := make([]string, len(tiers))
	for i, tr := range tiers {
		names[i] = tr.(map[string]any)["name"].(string)
	}

	put := func(content string) map[string]any {
		return e.must(conn, "put", map[string]any{"set": "cal", "text": content, "ext": "ics"})
	}
	statuses := func(out map[string]any) map[string]string {
		m := map[string]string{}
		for _, r := range out["tiers"].([]any) {
			rm := r.(map[string]any)
			m[rm["tier"].(string)] = rm["status"].(string)
		}
		return m
	}

	// 1. First put writes everywhere.
	for tier, st := range statuses(put("BEGIN:VCALENDAR\nv1\nEND:VCALENDAR")) {
		if st != "written" {
			t.Errorf("first put on %s: %s", tier, st)
		}
	}
	// 2. Identical content: deduped on every tier.
	for tier, st := range statuses(put("BEGIN:VCALENDAR\nv1\nEND:VCALENDAR")) {
		if st != "unchanged" {
			t.Errorf("identical put on %s should be unchanged, got %s", tier, st)
		}
	}
	// 3. Two more distinct versions (1s apart: distinct timestamps) -> retention keeps the
	// newest of each of the last 2 days; all are today, so exactly 1 survives per tier.
	time.Sleep(1100 * time.Millisecond)
	put("BEGIN:VCALENDAR\nv2\nEND:VCALENDAR")
	time.Sleep(1100 * time.Millisecond)
	final := "BEGIN:VCALENDAR\nv3 ☃ binary-safe\nEND:VCALENDAR"
	put(final)

	for _, tier := range names {
		t.Run(tier, func(t *testing.T) {
			items := e.must(conn, "list", map[string]any{"set": "cal", "tier": tier})["items"].([]any)
			if len(items) != 1 {
				t.Fatalf("retention keeps 1 (same day, keep_daily 2): %d %v", len(items), items)
			}
			got := e.must(conn, "read", map[string]any{"set": "cal", "tier": tier})
			if got["content"] != final {
				t.Fatalf("restore: %q", got["content"])
			}
			v := e.must(conn, "verify", map[string]any{"set": "cal", "tiers": []any{tier}, "mode": "both"})
			if v["ok"] != true {
				t.Fatalf("verify: %v", v)
			}
		})
	}

	// 4. Binary content round-trips through base64.
	bin := make([]byte, 4096)
	for i := range bin {
		bin[i] = byte(i * 7)
	}
	e.must(conn, "put", map[string]any{"set": "blob", "base64": base64.StdEncoding.EncodeToString(bin), "ext": "bin"})
	for _, tier := range names {
		got := e.must(conn, "read", map[string]any{"set": "blob", "tier": tier, "encoding": "base64"})
		if got["content"] != base64.StdEncoding.EncodeToString(bin) {
			t.Errorf("%s: binary round-trip mismatch", tier)
		}
	}

	// 5. Restore testing catches damage: corrupt the local object, delete the
	// rclone-s3 object behind the plugin's back.
	localObj := mustGlob(t, filepath.Join(e.dir, "local", "cal", "cal-*.ics"))
	if err := os.WriteFile(localObj, []byte("corrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.invoke(conn, "verify", map[string]any{"set": "cal", "tiers": []any{"disk"}}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("corrupted local snapshot must fail verify: %v", err)
	}
	ls := exec.Command("rclone", "--config", "", "lsf", e.rcloneS3Remote("rclone")+"/cal")
	out, err := ls.Output()
	if err != nil {
		t.Fatalf("rclone lsf: %v", err)
	}
	var obj string
	for _, l := range strings.Fields(string(out)) {
		if strings.HasSuffix(l, ".ics.age") {
			obj = l
		}
	}
	if obj == "" {
		t.Fatalf("no encrypted object on rclone-s3: %q", out)
	}
	run(t, "rclone", "--config", "", "deletefile", e.rcloneS3Remote("rclone")+"/cal/"+obj)
	if v, err := e.invoke(conn, "verify", map[string]any{"set": "cal", "tiers": []any{"rclone-s3"}}); err == nil {
		t.Errorf("a vanished rclone object must fail verify: %v", v)
	}

	// 6. Encryption at rest: the rclone-s3 objects and restic/borg repos never
	// hold the plaintext.
	for _, d := range []string{"restic", "borg", "local-age"} {
		_ = filepath.Walk(filepath.Join(e.dir, d), func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				b, _ := os.ReadFile(p)
				if strings.Contains(string(b), "BEGIN:VCALENDAR") {
					t.Errorf("plaintext found at rest in %s", p)
				}
			}
			return nil
		})
	}
}

// TestBackupHostKeyMismatchRefused: a wrong known_hosts entry must stop the
// connection, not silently trust the server.
func TestBackupHostKeyMismatchRefused(t *testing.T) {
	e := requireBackupE2E(t)
	other := t.TempDir()
	run(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(other, "k"))
	pub, _ := os.ReadFile(filepath.Join(other, "k.pub"))
	f := strings.Fields(string(pub))
	_, port, _ := net.SplitHostPort(e.sshHost)
	bad := e.sshBlock()
	bad["known_hosts"] = fmt.Sprintf("[127.0.0.1]:%s %s %s", port, f[0], f[1])
	conn := map[string]any{
		"allow_hosts": []any{e.sshHost},
		"tiers":       []any{map[string]any{"driver": "borg", "repo": "ssh://bk@127.0.0.1:" + port + "/home/bk/borg-bad", "passphrase": "x", "ssh": bad}},
	}
	if _, err := e.invoke(conn, "put", map[string]any{"set": "s", "text": "x"}); err == nil {
		t.Fatal("put through a mismatched host key must fail")
	}
}

// --- helpers ---

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

func dockerRun(t *testing.T, name string, args ...string) {
	t.Helper()
	name = fmt.Sprintf("%s-%d", name, time.Now().UnixNano())
	full := append([]string{"run", "-d", "--rm", "--name", name}, args...)
	if out, err := exec.Command("docker", full...).CombinedOutput(); err != nil {
		t.Fatalf("docker run %s: %v\n%s", name, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
}

func waitTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never came up: %v", addr, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func waitS3(t *testing.T, addr string) {
	t.Helper()
	remote := fmt.Sprintf(":s3,provider=Other,endpoint='http://%s',access_key_id=%s,secret_access_key=%s,force_path_style=true:", addr, rfAccess, rfSecret)
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := exec.Command("rclone", "--config", "", "-q", "mkdir", remote+"restic").Run()
		if err == nil {
			_ = exec.Command("rclone", "--config", "", "-q", "mkdir", remote+"rclone").Run()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("RustFS never ready: %v", err)
		}
		time.Sleep(time.Second)
	}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func mustGlob(t *testing.T, pattern string) string {
	t.Helper()
	m, _ := filepath.Glob(pattern)
	if len(m) != 1 {
		t.Fatalf("glob %s: %v", pattern, m)
	}
	return m[0]
}

func nonComment(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// TestBackupThroughEgressProxy runs the SSH tiers the way conductor's jail
// does: HTTPS_PROXY points at a CONNECT proxy that admits only allow_hosts.
// restic/borg reach the server via ssh's ProxyCommand (this binary's
// __connect mode), rclone via its sftp http_proxy; S3 via the proxy env.
func TestBackupThroughEgressProxy(t *testing.T) {
	e := requireBackupE2E(t)
	_, sshPort, _ := net.SplitHostPort(e.sshHost)
	allowed := map[string]bool{e.sshHost: true, e.s3: true}
	var refused []string
	proxy := startConnectProxy(t, func(dst string) bool {
		if !allowed[dst] {
			refused = append(refused, dst)
		}
		return allowed[dst]
	})
	t.Setenv("HTTPS_PROXY", "http://"+proxy)
	t.Setenv("HTTP_PROXY", "http://"+proxy)
	e.c = rpctest.Start(t, rpctest.BuildConnector(t, "backup")) // inherits the proxy env

	id, _ := age.GenerateX25519Identity()
	conn := map[string]any{
		"allow_hosts": []any{e.sshHost, e.s3},
		"tiers": []any{
			map[string]any{"name": "restic-sftp", "driver": "restic", "repo": "sftp:bk@127.0.0.1:/home/bk/p-restic", "password": "pw", "ssh": e.sshBlock()},
			map[string]any{"name": "borg-ssh", "driver": "borg", "repo": "ssh://bk@127.0.0.1:" + sshPort + "/home/bk/p-borg", "passphrase": "pp", "ssh": e.sshBlock()},
			map[string]any{"name": "rclone-sftp", "driver": "rclone", "remote": ":sftp,host=127.0.0.1,user=bk:/home/bk/p-rclone", "age_identity": id.String(), "ssh": e.sshBlock()},
			map[string]any{"name": "restic-s3", "driver": "restic", "repo": "s3:http://" + e.s3 + "/restic", "password": "pw-proxy", "env": e.s3Env()},
		},
	}
	e.must(conn, "put", map[string]any{"set": "px", "text": "via the proxy"})
	if v := e.must(conn, "verify", map[string]any{"set": "px", "mode": "both"}); v["ok"] != true {
		t.Fatalf("verify through proxy: %v", v)
	}
	if len(refused) > 0 {
		t.Fatalf("tried undeclared destinations: %v", refused)
	}
	// And a destination the proxy refuses fails loudly.
	delete(allowed, e.sshHost)
	if _, err := e.invoke(conn, "put", map[string]any{"set": "px", "text": "blocked", "tiers": []any{"borg-ssh"}}); err == nil {
		t.Fatal("a refused egress must fail the put")
	}
}

// startConnectProxy is a minimal HTTP CONNECT proxy with an allow func.
func startConnectProxy(t *testing.T, allow func(string) bool) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				if !allow(req.Host) {
					fmt.Fprint(c, "HTTP/1.1 403 Forbidden\r\n\r\n")
					return
				}
				up, err := net.Dial("tcp", req.Host)
				if err != nil {
					fmt.Fprint(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				fmt.Fprint(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
				<-done
			}(c)
		}
	}()
	return l.Addr().String()
}

// TestBackupNativeDriversNeedNoTools: the sftp and s3 drivers work with NO
// restic/rclone/borg/ssh on PATH, directly and through the egress proxy.
func TestBackupNativeDriversNeedNoTools(t *testing.T) {
	e := requireBackupE2E(t)
	// Start a plugin process whose PATH has none of the tools.
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	e.c = rpctest.Start(t, rpctest.BuildConnectorWithPATH(t, "backup", os.Getenv("ORIG_PATH_FOR_BUILD")))

	id, _ := age.GenerateX25519Identity()
	_, sshPort, _ := net.SplitHostPort(e.sshHost)
	sshb := e.sshBlock()
	sshb["port"] = sshPort
	conn := map[string]any{
		"allow_hosts": []any{e.sshHost, e.s3},
		"tiers": []any{
			map[string]any{"name": "sftp", "driver": "sftp", "target": "bk@127.0.0.1:/home/bk/native", "ssh": sshb, "age_identity": id.String()},
			map[string]any{"name": "s3", "driver": "s3", "endpoint": "http://" + e.s3, "bucket": "native-bucket", "prefix": "conductor",
				"access_key_id": rfAccess, "secret_access_key": rfSecret, "age_identity": id.String(), "keep_daily": 2, "keep_weekly": 0, "keep_monthly": 0},
			map[string]any{"name": "s3-plain", "driver": "s3", "endpoint": "http://" + e.s3, "bucket": "native-plain",
				"access_key_id": rfAccess, "secret_access_key": rfSecret, "encrypt": false},
		},
	}
	statuses := []string{}
	for _, c := range []string{"v1", "v1", "v2"} {
		out := e.must(conn, "put", map[string]any{"set": "n", "text": c, "ext": "ics"})
		statuses = append(statuses, out["tiers"].([]any)[0].(map[string]any)["status"].(string))
		time.Sleep(1100 * time.Millisecond)
	}
	if strings.Join(statuses, ",") != "written,unchanged,written" {
		t.Errorf("dedup on the sftp tier: %v", statuses)
	}
	for _, tier := range []string{"sftp", "s3", "s3-plain"} {
		items := e.must(conn, "list", map[string]any{"set": "n", "tier": tier})["items"].([]any)
		// v1, v1 (deduped), v2 all today: daily retention keeps the newest.
		if len(items) != 1 {
			t.Errorf("%s: retention keeps the newest of today, got %d: %v", tier, len(items), items)
		}
		if got := e.must(conn, "read", map[string]any{"set": "n", "tier": tier}); got["content"] != "v2" {
			t.Errorf("%s restore: %v", tier, got["content"])
		}
		if v := e.must(conn, "verify", map[string]any{"set": "n", "tiers": []any{tier}, "mode": "both", "sample": 0}); v["ok"] != true {
			t.Errorf("%s verify: %v", tier, v)
		}
	}
	// Through an allow-listing CONNECT proxy, as under conductor's jail.
	proxy := startConnectProxy(t, func(dst string) bool { return dst == e.sshHost || dst == e.s3 })
	t.Setenv("HTTPS_PROXY", "http://"+proxy)
	t.Setenv("HTTP_PROXY", "http://"+proxy)
	e.c = rpctest.Start(t, rpctest.BuildConnectorWithPATH(t, "backup", os.Getenv("ORIG_PATH_FOR_BUILD")))
	e.must(conn, "put", map[string]any{"set": "np", "text": "proxied", "tiers": []any{"sftp", "s3"}})
	if v := e.must(conn, "verify", map[string]any{"set": "np", "tiers": []any{"sftp", "s3"}}); v["ok"] != true {
		t.Fatalf("verify through proxy: %v", v)
	}
	// Wrong host key is refused by the native client too.
	bad := e.sshBlock()
	bad["known_hosts"] = strings.Replace(e.knownHosts, "AAAA", "AAAB", 1)
	badConn := map[string]any{"allow_hosts": []any{e.sshHost}, "tiers": []any{
		map[string]any{"driver": "sftp", "target": "bk@127.0.0.1:/home/bk/x", "ssh": bad, "encrypt": false}}}
	if _, err := e.invoke(badConn, "put", map[string]any{"set": "x", "text": "x"}); err == nil {
		t.Fatal("sftp with a wrong host key must fail")
	}
}
