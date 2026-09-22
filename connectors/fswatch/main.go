// Command conductor-fswatch is a filesystem-watch SOURCE connector for
// conductor, built against the public plugin SDK. It fires a trigger when a
// matching file settles in a watched directory — the low-latency counterpart to
// a cron sweep. Matches are debounced so one download's burst of events collapses
// into a single trigger; new subdirectories are followed (fsnotify is not
// recursive). Keep a cron sweep alongside it: inotify is lossy (events missed
// while the process is down, dropped on queue overflow, uneven on some FUSE
// mounts).
//
// Connection:
//
//	watches:
//	  staged: { path: /srv/incoming, match: "*.m4b", debounce: 15s, recursive: true }
//
// A trigger uses `on: <instance>.<watch>`; steps read {{.path}} (full path),
// {{.file}} (basename — NOT {{.name}}, which is the reserved repo-name key),
// {{.op}}, {{.watch}}.
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
	"github.com/fsnotify/fsnotify"
)

const defaultDebounce = 15 * time.Second

type fswatchPlugin struct{}

func (fswatchPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "fswatch",
		Desc: "Filesystem watch: fires when a matching file settles in a watched directory; no verbs.",
		Connection: plugin.Schema{
			"watches": {Type: "map", Required: true, Desc: "name -> { path, match, debounce, recursive, events }"},
		},
		Events: []plugin.Event{{
			Name:    "<watch>",
			Dynamic: true,
			Desc:    "a matching file settled under a watch",
			Context: plugin.Schema{
				"watch": {Type: "string"},
				"path":  {Type: "string"},
				"file":  {Type: "string", Desc: "basename of the changed file (not `name` — that reserved key is the repo name)"},
				"op":    {Type: "string"},
			},
			Filters: plugin.Schema{
				"file": {Type: "string", Desc: "match this exact basename"},
			},
		}},
	}
}

func (fswatchPlugin) Invoke(plugin.InvokeRequest) (plugin.InvokeResult, error) {
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "fswatch has no verbs")
}

// watch is one resolved watch from the connection config.
type watch struct {
	name      string
	path      string
	match     string // glob on basename; "" or "*" matches all
	debounce  time.Duration
	recursive bool
	ops       fsnotify.Op
}

func (w watch) matches(name string, op fsnotify.Op) bool {
	if op&w.ops == 0 {
		return false
	}
	if w.match == "" || w.match == "*" {
		return true
	}
	ok, err := filepath.Match(w.match, name)
	return err == nil && ok
}

// StartSource watches every configured directory tree and emits a debounced
// event per watch as matching files settle. Returns when ctx is cancelled.
func (fswatchPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	watches, err := parseWatches(req.Config["watches"])
	if err != nil {
		return err
	}
	if len(watches) == 0 {
		return fmt.Errorf("fswatch[%s]: no watches configured", req.Instance)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fswatch[%s]: new watcher: %w", req.Instance, err)
	}
	defer w.Close()

	// emit is called from debounce timer goroutines; serialize it.
	var emitMu sync.Mutex
	safeEmit := func(payload any) {
		emitMu.Lock()
		defer emitMu.Unlock()
		_ = emit(payload)
	}

	dirWatch := map[string]int{} // watched dir -> watch index
	deb := newDebouncer(watches, func(idx int, path, op string) {
		wc := watches[idx]
		safeEmit(map[string]any{
			"event": wc.name,
			"title": "fswatch: " + req.Instance + "/" + wc.name,
			"context": map[string]any{
				"watch": wc.name,
				"path":  path,
				"file":  filepath.Base(path),
				"op":    op,
			},
		})
	})

	add := func(dir string, idx int) {
		if err := w.Add(dir); err != nil {
			log.Printf("fswatch[%s]: cannot watch %s: %v", req.Instance, dir, err)
			return
		}
		dirWatch[dir] = idx
	}
	var addTree func(dir string, idx int)
	addTree = func(dir string, idx int) {
		add(dir, idx)
		if !watches[idx].recursive {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() {
				addTree(filepath.Join(dir, e.Name()), idx)
			}
		}
	}
	for i, wc := range watches {
		info, err := os.Stat(wc.path)
		if err != nil || !info.IsDir() {
			log.Printf("fswatch[%s]: watch %q: %s is not a directory (yet); skipping", req.Instance, wc.name, wc.path)
			continue
		}
		addTree(wc.path, i)
	}
	log.Printf("fswatch[%s]: %d watch(es), %d dir(s)", req.Instance, len(watches), len(dirWatch))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			idx, known := dirWatch[filepath.Dir(ev.Name)]
			if !known {
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					if watches[idx].recursive {
						addTree(ev.Name, idx)
						deb.arm(idx, ev.Name, ev.Op.String())
					}
					continue
				}
			}
			if watches[idx].matches(filepath.Base(ev.Name), ev.Op) {
				deb.arm(idx, ev.Name, ev.Op.String())
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			log.Printf("fswatch[%s]: watcher error: %v — re-arming all watches", req.Instance, err)
			for i := range watches {
				deb.arm(i, watches[i].path, "error")
			}
		}
	}
}

func main() {
	if err := plugin.Serve(fswatchPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "fswatch: serve: %v\n", err)
		os.Exit(1)
	}
}
