// Command conductor-logwatch is a log-line SOURCE connector for conductor, built
// against the public plugin SDK. It streams a local log — a file followed
// through rotation (via `tail -n0 -F`), or the stdout of a streaming command —
// and fires a trigger when a line matches a regexp. "When this line appears, do
// X." Matches are debounced so one error's multi-line burst collapses into a
// single trigger.
//
// There is no journald-specific backend; point a watch's `command:` at
// `journalctl -f -u <unit> -o cat` (also covers `docker logs -f`, `kubectl logs
// -f`, anything that streams lines to stdout).
//
// Connection:
//
//	watches:
//	  app-errors: { path: /var/log/app.log, pattern: 'ERROR (?P<msg>.*)' }
//	  unit-oom:   { command: [journalctl, -f, -u, myservice, -o, cat], pattern: 'Out of memory' }
//
// A trigger uses `on: <instance>.<watch>`; steps read {{.line}}, {{.source}},
// {{.watch}}, and named captures via {{.groups.<name>}}.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

const (
	defaultDebounce = 2 * time.Second
	respawnBackoff  = 2 * time.Second
	maxLine         = 1 << 20
)

type logwatchPlugin struct{}

func (logwatchPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "logwatch",
		Desc: "Log watch: fires when a line matching a regexp appears in a followed file or a streaming command's stdout; no verbs.",
		Connection: plugin.Schema{
			"watches": {Type: "map", Required: true, Desc: "name -> { path | command, pattern, debounce }"},
		},
		Events: []plugin.Event{{
			Name:    "<watch>",
			Dynamic: true,
			Desc:    "a line matched the watch's pattern",
			Context: plugin.Schema{
				"watch":  {Type: "string"},
				"line":   {Type: "string", Desc: "the full matched line"},
				"source": {Type: "string", Desc: "the file path, or \"command\""},
				"groups": {Type: "map", Desc: "named regexp capture groups from the match"},
			},
		}},
	}
}

func (logwatchPlugin) Invoke(plugin.InvokeRequest) (plugin.InvokeResult, error) {
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "logwatch has no verbs")
}

// StartSource streams every watch and emits a debounced event per watch as
// matching lines arrive. Returns when ctx is cancelled.
func (logwatchPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	watches, err := parseWatches(req.Config["watches"])
	if err != nil {
		return err
	}
	if len(watches) == 0 {
		return fmt.Errorf("logwatch[%s]: no watches configured", req.Instance)
	}

	var emitMu sync.Mutex
	safeEmit := func(payload any) {
		emitMu.Lock()
		defer emitMu.Unlock()
		_ = emit(payload)
	}
	deb := newDebouncer(watches, func(idx int, ev logLine) {
		wc := watches[idx]
		safeEmit(map[string]any{
			"event": wc.name,
			"title": "logwatch: " + req.Instance + "/" + wc.name,
			"context": map[string]any{
				"watch":  wc.name,
				"line":   ev.line,
				"source": wc.source(),
				"groups": ev.groups,
			},
		})
	})

	var wg sync.WaitGroup
	for i, wc := range watches {
		wg.Add(1)
		go func(idx int, wc watch) {
			defer wg.Done()
			stream(ctx, req.Instance, idx, wc, deb)
		}(i, wc)
	}
	log.Printf("logwatch[%s]: %d watch(es) streaming", req.Instance, len(watches))

	<-ctx.Done()
	wg.Wait()
	return ctx.Err()
}

// stream runs one watch's command, scanning stdout for matching lines, and
// respawns (with backoff) if it exits — a log source must be durable. Exits when
// ctx is cancelled.
func stream(ctx context.Context, instance string, idx int, wc watch, deb *debouncer) {
	argv := wc.argv()
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("logwatch[%s]: watch %q: stdout pipe: %v", instance, wc.name, err)
			return
		}
		if err := cmd.Start(); err != nil {
			log.Printf("logwatch[%s]: watch %q: start %v: %v", instance, wc.name, argv, err)
			if !sleepCtx(ctx, respawnBackoff) {
				return
			}
			continue
		}
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), maxLine)
		for sc.Scan() {
			line := sc.Text()
			if m := wc.re.FindStringSubmatch(line); m != nil {
				deb.arm(idx, logLine{line: line, groups: namedGroups(wc.re, m)})
			}
		}
		_ = cmd.Wait()
		if ctx.Err() != nil {
			return
		}
		log.Printf("logwatch[%s]: watch %q: stream ended, respawning in %s", instance, wc.name, respawnBackoff)
		if !sleepCtx(ctx, respawnBackoff) {
			return
		}
	}
}

func namedGroups(re *regexp.Regexp, match []string) map[string]string {
	names := re.SubexpNames()
	out := map[string]string{}
	for i, name := range names {
		if i == 0 || name == "" || i >= len(match) {
			continue
		}
		out[name] = match[i]
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func main() {
	if err := plugin.Serve(logwatchPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "logwatch: serve: %v\n", err)
		os.Exit(1)
	}
}
