package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// parseWatches decodes the `watches:` connection map (delivered as map[string]any
// over the wire) into resolved watch structs, sorted by name for determinism.
func parseWatches(raw any) ([]watch, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("fswatch: `watches` must be a map of name -> spec")
	}
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]watch, 0, len(names))
	for _, name := range names {
		spec, ok := m[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("fswatch: watch %q: spec must be a map", name)
		}
		path, _ := spec["path"].(string)
		if path == "" {
			return nil, fmt.Errorf("fswatch: watch %q: `path` is required", name)
		}
		w := watch{
			name:      name,
			path:      path,
			match:     str(spec["match"]),
			debounce:  durationOf(spec["debounce"], defaultDebounce),
			recursive: boolOr(spec["recursive"], true),
			ops:       opsOf(spec["events"]),
		}
		out = append(out, w)
	}
	return out, nil
}

func str(v any) string { s, _ := v.(string); return s }

func boolOr(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

// durationOf accepts a Go duration string ("15s") or a number of seconds.
func durationOf(v any, def time.Duration) time.Duration {
	switch t := v.(type) {
	case string:
		if t == "" {
			return def
		}
		if d, err := time.ParseDuration(t); err == nil && d > 0 {
			return d
		}
	case float64:
		if t > 0 {
			return time.Duration(t * float64(time.Second))
		}
	case int:
		if t > 0 {
			return time.Duration(t) * time.Second
		}
	}
	return def
}

// opsOf turns an events: list into an fsnotify op mask; empty => create+write+rename.
func opsOf(v any) fsnotify.Op {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return fsnotify.Create | fsnotify.Write | fsnotify.Rename
	}
	var m fsnotify.Op
	for _, e := range list {
		switch strings.ToLower(strings.TrimSpace(str(e))) {
		case "create":
			m |= fsnotify.Create
		case "write":
			m |= fsnotify.Write
		case "rename":
			m |= fsnotify.Rename
		case "remove":
			m |= fsnotify.Remove
		case "chmod":
			m |= fsnotify.Chmod
		}
	}
	return m
}

// debouncer coalesces a burst of events per watch into one fire once the watch
// has been quiet for its debounce window.
type debouncer struct {
	watches []watch
	fireFn  func(idx int, path, op string)
	states  []*watchState
}

type watchState struct {
	mu       sync.Mutex
	timer    *time.Timer
	lastPath string
	lastOp   string
}

func newDebouncer(watches []watch, fire func(idx int, path, op string)) *debouncer {
	states := make([]*watchState, len(watches))
	for i := range states {
		states[i] = &watchState{}
	}
	return &debouncer{watches: watches, fireFn: fire, states: states}
}

func (d *debouncer) arm(idx int, path, op string) {
	st := d.states[idx]
	dur := d.watches[idx].debounce
	st.mu.Lock()
	defer st.mu.Unlock()
	st.lastPath, st.lastOp = path, op
	if st.timer == nil {
		st.timer = time.AfterFunc(dur, func() {
			st.mu.Lock()
			p, o := st.lastPath, st.lastOp
			st.mu.Unlock()
			d.fireFn(idx, p, o)
		})
	} else {
		st.timer.Reset(dur)
	}
}
