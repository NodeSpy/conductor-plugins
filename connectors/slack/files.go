package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// thread/download verbs (#161 parity): read a message's thread, or stage its
// files on disk. Ported from internal/connector/slack_files.go; the only
// change is WHERE files land — a plugin has no conductor state dir, so it
// uses its own staging root (Q7: "the host gives each instance a staging
// directory inside its fs capability" — this plugin manages its own until a
// host-provided one exists; see stagingRoot).

const (
	defaultThreadLimit = 200
	maxThreadLimit     = 1000

	defaultMaxFiles      = 10
	capMaxFiles          = 50
	defaultMaxFileBytes  = 20 << 20
	capMaxFileBytes      = 50 << 20
	defaultMaxTotalBytes = 50 << 20
	capMaxTotalBytes     = 200 << 20

	// stagedFileTTL: staging dirs older than this are pruned on the next
	// download.
	stagedFileTTL = 7 * 24 * time.Hour
)

// slackMessage is the subset of a conversations.replies message read here.
type slackMessage struct {
	User     string `json:"user"`
	BotID    string `json:"bot_id"`
	Username string `json:"username"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts"`
	Files    []struct {
		ID                 string `json:"id"`
		Name               string `json:"name"`
		Mimetype           string `json:"mimetype"`
		Size               int64  `json:"size"`
		Mode               string `json:"mode"`
		URLPrivate         string `json:"url_private"`
		URLPrivateDownload string `json:"url_private_download"`
	} `json:"files"`
}

// intOpt reads an integer option with a default and a hard cap.
func intOpt(opts map[string]any, key string, def, max int) int {
	n := def
	switch v := opts[key].(type) {
	case int:
		n = v
	case int64:
		n = int(v)
	case float64:
		n = int(v)
	}
	if n <= 0 {
		n = def
	}
	if n > max {
		n = max
	}
	return n
}

func (p *Plugin) threadVerb(ctx context.Context, api *slackAPI, opts map[string]any) (map[string]any, error) {
	channel, _ := opts["channel"].(string)
	ts, _ := opts["ts"].(string)
	if channel == "" || ts == "" {
		return nil, fmt.Errorf("slack.thread: options.channel and ts are required")
	}
	msgs, truncated, err := api.replies(ctx, channel, ts, intOpt(opts, "limit", defaultThreadLimit, maxThreadLimit))
	if err != nil {
		return nil, fmt.Errorf("slack.thread: %w", err)
	}
	var list []any
	var text strings.Builder
	threadTS := ts
	for i, m := range msgs {
		if i == 0 && m.ThreadTS != "" {
			threadTS = m.ThreadTS
		}
		name := api.userName(ctx, m.User)
		if name == "" || name == m.User {
			name = firstNonEmptyStr(m.Username, "bot")
		}
		var files []any
		var fileNames []string
		for _, f := range m.Files {
			files = append(files, map[string]any{"id": f.ID, "name": f.Name, "mimetype": f.Mimetype, "size": f.Size})
			fileNames = append(fileNames, f.Name)
		}
		list = append(list, map[string]any{"user": m.User, "user_name": name, "text": m.Text, "ts": m.TS, "files": files})
		fmt.Fprintf(&text, "%s (%s):\n%s\n", name, m.TS, m.Text)
		if len(fileNames) > 0 {
			fmt.Fprintf(&text, "[files: %s]\n", strings.Join(fileNames, ", "))
		}
		text.WriteString("\n")
	}
	return map[string]any{
		"messages": list, "text": strings.TrimRight(text.String(), "\n"), "count": len(list),
		"permalink": api.permalink(ctx, channel, ts), "thread_ts": threadTS, "truncated": truncated,
	}, nil
}

func firstNonEmptyStr(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// unsafeNameRe matches every character a staged file name may not keep.
var unsafeNameRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeFileName reduces a Slack-supplied file name to a safe base name: no
// directory components, no characters outside [A-Za-z0-9._-] (so no template
// braces, quotes, or spaces either), no leading dots/dashes, at most 100
// bytes with the extension kept.
func sanitizeFileName(name, fallback string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base("/" + name)
	name = unsafeNameRe.ReplaceAllString(name, "_")
	name = strings.TrimLeft(name, ".-_")
	if len(name) > 100 {
		ext := filepath.Ext(name)
		if len(ext) > 10 {
			ext = ""
		}
		name = name[:100-len(ext)] + ext
	}
	if name == "" || name == "." {
		name = unsafeNameRe.ReplaceAllString(fallback, "_")
	}
	if name == "" {
		name = "file"
	}
	return name
}

// stagingRoot is the FALLBACK staging location (overridable in tests), used
// only when the host gives the instance none (InvokeRequest.Staging empty —
// an older host, or one not yet carrying Q7's staging directory). A plugin
// has no conductor state dir to anchor to in that case, so it uses its own
// cache-style directory under the user's cache dir, falling back to the OS
// temp dir.
var stagingRoot = func() string {
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "conductor-slack", "slack-files")
	}
	return filepath.Join(os.TempDir(), "conductor-slack-files")
}

var errTooLarge = errors.New("larger than max_file_bytes")

// fetchFile downloads one private file to path, refusing more than max bytes.
func fetchFile(ctx context.Context, a *slackAPI, rawURL, path string, max int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.botToken)
	client := &http.Client{Timeout: 2 * time.Minute, Transport: a.httpc.Transport,
		// Never carry the bot token off Slack: a redirect must stay on an
		// allowed host (Go drops Authorization cross-host anyway).
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) > 5 || !a.fileURLAllowed(r.URL.String()) {
				return fmt.Errorf("redirect to %s refused", r.URL.Host)
			}
			return nil
		}}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		// Slack answers a token without files:read with its HTML login page.
		return 0, fmt.Errorf("got an HTML page instead of the file (does the bot token have files:read?)")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = errTooLarge
	}
	if err != nil {
		_ = os.Remove(path)
		return 0, err
	}
	return n, nil
}

func isImage(mime string) bool { return strings.HasPrefix(strings.ToLower(mime), "image/") }

// downloadVerb stages a message's (or its thread's) files on disk. staging is
// InvokeRequest.Staging — the per-instance directory the HOST made,
// writable in the plugin's sandbox (plugin-contract.md Q7); an engine accepts
// a templated images: path only when it resolves (symlinks included) under
// the host's staging root. When the host gives none (an older host), it
// falls back to the plugin's own stagingRoot().
func (p *Plugin) downloadVerb(ctx context.Context, api *slackAPI, opts map[string]any, staging string) (map[string]any, error) {
	channel, _ := opts["channel"].(string)
	ts, _ := opts["ts"].(string)
	if channel == "" || ts == "" {
		return nil, fmt.Errorf("slack.download: options.channel and ts are required")
	}
	wholeThread := true
	if v, ok := opts["thread"].(bool); ok {
		wholeThread = v
	}
	maxFiles := intOpt(opts, "max_files", defaultMaxFiles, capMaxFiles)
	maxFile := int64(intOpt(opts, "max_file_bytes", defaultMaxFileBytes, capMaxFileBytes))
	maxTotal := int64(intOpt(opts, "max_total_bytes", defaultMaxTotalBytes, capMaxTotalBytes))

	msgs, _, err := api.replies(ctx, channel, ts, maxThreadLimit)
	if err != nil {
		return nil, fmt.Errorf("slack.download: %w", err)
	}
	if !wholeThread {
		var only []slackMessage
		for _, m := range msgs {
			if m.TS == ts {
				only = append(only, m)
			}
		}
		msgs = only
	}

	root := staging
	if root == "" {
		root = stagingRoot()
	}
	dir := filepath.Join(root, sanitizeFileName(channel, "channel")+"-"+sanitizeFileName(ts, "ts"))
	if rel, err := filepath.Rel(root, dir); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("slack.download: bad staging dir for %s/%s", channel, ts)
	}
	pruneStaging(root, dir)
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("slack.download: reset %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("slack.download: %w", err)
	}

	files, paths, images, skipped := []any{}, []any{}, []any{}, []any{}
	var total int64
	n := 0
	for _, m := range msgs {
		for _, f := range m.Files {
			skip := func(reason string) {
				skipped = append(skipped, map[string]any{"name": f.Name, "reason": reason})
			}
			src := firstNonEmptyStr(f.URLPrivateDownload, f.URLPrivate)
			switch {
			case f.Mode == "external" || f.Mode == "tombstone" || f.Mode == "hidden_by_limit" || src == "":
				skip("not downloadable (" + firstNonEmptyStr(f.Mode, "no url") + ")")
				continue
			case n >= maxFiles:
				skip("max_files reached")
				continue
			case f.Size > maxFile:
				skip("larger than max_file_bytes")
				continue
			case total+f.Size > maxTotal:
				skip("max_total_bytes reached")
				continue
			case !api.fileURLAllowed(src):
				skip("file URL is not on slack.com")
				continue
			}
			name := fmt.Sprintf("%02d-%s", n+1, sanitizeFileName(f.Name, f.ID))
			path := filepath.Join(dir, name)
			if rel, err := filepath.Rel(dir, path); err != nil || strings.Contains(rel, string(filepath.Separator)) || strings.HasPrefix(rel, "..") {
				skip("unsafe name")
				continue
			}
			limit := maxFile
			if rest := maxTotal - total; rest < limit {
				limit = rest
			}
			size, err := api.fetchFile(ctx, src, path, limit)
			if err != nil {
				skip(err.Error())
				continue
			}
			n++
			total += size
			files = append(files, map[string]any{"name": name, "path": path, "mimetype": f.Mimetype, "size": size})
			paths = append(paths, path)
			if isImage(f.Mimetype) {
				images = append(images, path)
			}
		}
	}
	return map[string]any{"dir": dir, "files": files, "paths": paths, "images": images, "skipped": skipped, "count": n}, nil
}

// pruneStaging removes staging dirs under root older than stagedFileTTL (best
// effort), except keep.
func pruneStaging(root, keep string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-stagedFileTTL)
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if !e.IsDir() || p == keep {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(p)
		}
	}
}
