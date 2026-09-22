// Command conductor-postgres is the PostgreSQL LISTEN/NOTIFY connector as an
// external conductor plugin. Its headline feature is a SOURCE: a persistent
// connection that LISTENs on one or more channels and emits one event per
// NOTIFY — turning a Postgres database (the same one the built-in `sql`
// connector queries) into an event source. A trigger on the table that calls
// pg_notify() becomes a conductor trigger; "react to a row change" needs no
// polling. It also exposes query/exec/notify verbs so the connector isn't
// source-only.
//
// Built on jackc/pgx/v5 (pure Go). LISTEN needs a dedicated session, so the
// source holds its own connection open (reconnecting with backoff and
// re-LISTENing on reconnect); the verbs open a short-lived connection per call.
//
// Connection (used for both Invoke and StartSource):
//
//	url: "postgres://user:pass@host:5432/db?sslmode=require"  # required
//	listen: ["row_changed", "jobs"]                           # channels to LISTEN on (StartSource only)
//
// stdout is the RPC transport; all logging goes to stderr.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

type pgPlugin struct{}

func (pgPlugin) Describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindConnector,
		Type: "postgres",
		Desc: "PostgreSQL LISTEN/NOTIFY as a source (one event per NOTIFY), plus query/exec/notify verbs. Turns a Postgres DB into an event source — react to a row change without polling.",
		Connection: plugin.Schema{
			"url":    {Type: "string", Required: true, Desc: "Postgres connection URL: postgres://user:pass@host:5432/db?sslmode=require"},
			"listen": {Type: "list", Desc: "channel names to LISTEN on (StartSource only)"},
		},
		Verbs: []plugin.Verb{
			{
				Name: "query", Desc: "run a SELECT (or any row-returning statement) and return the rows",
				Options: plugin.Schema{
					"sql":  {Type: "string", Required: true, Desc: "the SQL; use $1, $2, … placeholders for args"},
					"args": {Type: "list", Desc: "positional arguments bound to $1, $2, …"},
				},
				Outputs: plugin.Schema{
					"rows":      {Type: "list", Desc: "the result rows, each a {column: value} map"},
					"row_count": {Type: "integer"},
				},
			},
			{
				Name: "exec", Desc: "run a statement that returns no rows (INSERT/UPDATE/DELETE/DDL)",
				Options: plugin.Schema{
					"sql":  {Type: "string", Required: true, Desc: "the SQL; use $1, $2, … placeholders for args"},
					"args": {Type: "list", Desc: "positional arguments bound to $1, $2, …"},
				},
				Outputs: plugin.Schema{
					"rows_affected": {Type: "integer"},
					"tag":           {Type: "string", Desc: "the command tag, e.g. \"UPDATE 3\""},
				},
			},
			{
				Name: "notify", Desc: "send a NOTIFY on a channel (pg_notify), to fan a message out to LISTENers",
				Options: plugin.Schema{
					"channel": {Type: "string", Required: true, Scope: "channel", Desc: "the channel to notify"},
					"payload": {Type: "string", Desc: "optional payload string"},
				},
				Outputs: plugin.Schema{"ok": {Type: "boolean"}},
			},
		},
		Events: []plugin.Event{
			{
				Name: "notification",
				Desc: "a NOTIFY arrived on a LISTENed channel",
				Context: plugin.Schema{
					"channel": {Type: "string", Desc: "the channel the NOTIFY was sent on"},
					"payload": {Type: "string", Desc: "the NOTIFY payload (empty string if none)"},
				},
				Filters: plugin.Schema{
					"channels": {Type: "list", Desc: "match any of these channels"},
					"channel":  {Type: "string", Desc: "match this exact channel"},
				},
			},
		},
		// The database host is operator-specific (config), so no fixed egress is
		// declared — narrow it per instance with network:. Spawns nothing.
		Capabilities: plugin.Capabilities{Spawns: false},
	}
}

func (pgPlugin) Invoke(req plugin.InvokeRequest) (plugin.InvokeResult, error) {
	url := str(req.Connection["url"])
	if url == "" {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "postgres: connection url is required")
	}
	o := req.Options
	if o == nil {
		o = map[string]any{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, req.Verb+": connect: "+err.Error())
	}
	defer conn.Close(context.Background())

	switch req.Verb {
	case "query":
		sql := str(o["sql"])
		if sql == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "query: sql is required")
		}
		rows, err := conn.Query(ctx, sql, anyList(o["args"])...)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "query: "+err.Error())
		}
		maps, err := pgx.CollectRows(rows, pgx.RowToMap)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "query: "+err.Error())
		}
		out := make([]any, len(maps))
		for i := range maps {
			out[i] = maps[i]
		}
		return plugin.InvokeResult{Outputs: map[string]any{"rows": out, "row_count": len(out)}}, nil

	case "exec":
		sql := str(o["sql"])
		if sql == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "exec: sql is required")
		}
		tag, err := conn.Exec(ctx, sql, anyList(o["args"])...)
		if err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "exec: "+err.Error())
		}
		return plugin.InvokeResult{Outputs: map[string]any{
			"rows_affected": tag.RowsAffected(),
			"tag":           tag.String(),
		}}, nil

	case "notify":
		channel := str(o["channel"])
		if channel == "" {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "notify: channel is required")
		}
		// pg_notify() takes the channel as a value, so the channel name is a
		// bound parameter — never string-concatenated into SQL.
		if _, err := conn.Exec(ctx, "SELECT pg_notify($1, $2)", channel, str(o["payload"])); err != nil {
			return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInternalError, "notify: "+err.Error())
		}
		return plugin.InvokeResult{Outputs: map[string]any{"ok": true}}, nil
	}
	return plugin.InvokeResult{}, plugin.Errorf(plugin.CodeInvalidParams, "postgres: unknown verb "+req.Verb)
}

func (pgPlugin) StartSource(ctx context.Context, req plugin.StartSourceRequest, emit func(any) error) error {
	cfg := req.Config
	url := str(cfg["url"])
	if url == "" {
		return fmt.Errorf("postgres: connection url is required")
	}
	channels := strList(cfg["listen"])
	if len(channels) == 0 {
		return fmt.Errorf("postgres: listen must name at least one channel")
	}
	// LISTEN takes an identifier, not a bound parameter, so validate each
	// channel name up front rather than interpolating an arbitrary string.
	for _, ch := range channels {
		if !validIdent(ch) {
			return fmt.Errorf("postgres: invalid channel name %q (must be a letter/underscore then letters, digits, or underscores)", ch)
		}
	}

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for ctx.Err() == nil {
		err := listenOnce(ctx, url, channels, req.Instance, emit)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "postgres[%s]: listen error: %v\n", req.Instance, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
	return nil
}

// listenOnce opens one connection, LISTENs on every channel, and blocks emitting
// a notification event per NOTIFY until the connection drops or ctx is
// cancelled. Reconnection/backoff is the caller's job.
func listenOnce(ctx context.Context, url string, channels []string, instance string, emit func(any) error) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	for _, ch := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
			return fmt.Errorf("LISTEN %s: %w", ch, err)
		}
	}
	fmt.Fprintf(os.Stderr, "postgres[%s]: listening on %v\n", instance, channels)

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err // ctx cancel or connection drop; caller decides
		}
		_ = emit(notificationEvent(n.Channel, n.Payload))
	}
}

func main() {
	if err := plugin.Serve(pgPlugin{}); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-postgres: %v\n", err)
		os.Exit(1)
	}
}

// notificationEvent builds the emit payload for one NOTIFY: the event name plus
// the channel/payload context the daemon filters and templates on. Pure and
// testable. `channels` is a singular-channel alias so filters: {channels: [...]}
// matches the daemon's list-contains evaluator.
func notificationEvent(channel, payload string) map[string]any {
	return map[string]any{
		"event": "notification",
		"kind":  "notification",
		"title": "pg NOTIFY " + channel,
		"context": map[string]any{
			"channel":  channel,
			"payload":  payload,
			"channels": channel, // singular alias for list-contains filtering
		},
	}
}

// validIdent reports whether s is a safe unquoted Postgres channel identifier:
// a letter or underscore, then letters, digits, or underscores.
func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// --- option helpers ---

func str(v any) string { s, _ := v.(string); return s }

// anyList returns the option as a []any (pgx query/exec args), accepting a
// single value or a list.
func anyList(v any) []any {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		return x
	default:
		return []any{x}
	}
}

// strList accepts a string, []string, or []any and returns a non-empty slice.
func strList(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
