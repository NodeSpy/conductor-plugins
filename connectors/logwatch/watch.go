package main

import (
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"
)

// watch is one resolved log watch. Exactly one of path / command is set.
type watch struct {
	name     string
	path     string
	command  []string
	re       *regexp.Regexp
	debounce time.Duration
}

// argv is the command a watch streams: an explicit command, or `tail -n0 -F` of
// a file (which follows the file through truncation and rotation).
func (w watch) argv() []string {
	if len(w.command) > 0 {
		return w.command
	}
	return []string{"tail", "-n", "0", "-F", w.path}
}

func (w watch) source() string {
	if len(w.command) > 0 {
		return "command"
	}
	return w.path
}

func parseWatches(raw any) ([]watch, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("logwatch: `watches` must be a map of name -> spec")
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
			return nil, fmt.Errorf("logwatch: watch %q: spec must be a map", name)
		}
		path, _ := spec["path"].(string)
		command := strList(spec["command"])
		if (path == "") == (len(command) == 0) {
			return nil, fmt.Errorf("logwatch: watch %q: set exactly one of `path` or `command`", name)
		}
		pattern, _ := spec["pattern"].(string)
		if pattern == "" {
			return nil, fmt.Errorf("logwatch: watch %q: `pattern` is required", name)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("logwatch: watch %q: bad pattern %q: %w", name, pattern, err)
		}
		out = append(out, watch{
			name:     name,
			path:     path,
			command:  command,
			re:       re,
			debounce: durationOf(spec["debounce"], defaultDebounce),
		})
	}
	return out, nil
}

func strList(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

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

// logLine is the per-watch debounce payload.
type logLine struct {
	line   string
	groups map[string]string
}

type debouncer struct {
	watches []watch
	fireFn  func(idx int, ev logLine)
	states  []*lwState
}

type lwState struct {
	mu    sync.Mutex
	timer *time.Timer
	last  logLine
}

func newDebouncer(watches []watch, fire func(idx int, ev logLine)) *debouncer {
	states := make([]*lwState, len(watches))
	for i := range states {
		states[i] = &lwState{}
	}
	return &debouncer{watches: watches, fireFn: fire, states: states}
}

func (d *debouncer) arm(idx int, ev logLine) {
	st := d.states[idx]
	dur := d.watches[idx].debounce
	st.mu.Lock()
	defer st.mu.Unlock()
	st.last = ev
	if st.timer == nil {
		st.timer = time.AfterFunc(dur, func() {
			st.mu.Lock()
			e := st.last
			st.mu.Unlock()
			d.fireFn(idx, e)
		})
	} else {
		st.timer.Reset(dur)
	}
}
