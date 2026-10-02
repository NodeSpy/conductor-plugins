// Command conductor-github is the GitHub connector as a standalone conductor
// plugin: every verb, every event, and the catch-up sweep of the github
// connector conductor bundles — the same implementation, not a second one.
//
// The handler is pkg/githubkit/ghplugin from conductor's public SDK: the shared
// github declaration, the verbs through pkg/githubkit, and the event source
// through pkg/githubkit/ghsource (webhook parsing, review folding with
// dispatch-once claims, the own-status guard, closed-PR drops, merge-state
// triggers, `me` identity, the unified filter, the adaptive sweep). It speaks
// the SDK's source extension (plugin.ConnectorABI): the daemon hands it the
// instance's triggers, it evaluates them and routes each event to the trigger
// it fired for, and the daemon nudges its sweep, forces events, and asks it for
// App tokens and target heads.
//
// Installed from the official repo and verified against its release, it is a
// trusted source by default: conductor treats its events as the builtin's —
// engine-interpreted kinds like new_comment and merge_conflict, and own-repo
// trust in their targets. A local build needs `trusted_source: true`. See
// docs/connectors/github.md.
//
// Built only against conductor's public pkg/ packages. stdout is the RPC
// transport; all logging goes to stderr.
package main

import (
	"fmt"
	"os"

	"github.com/NodeSpy/conductor/pkg/githubkit/ghplugin"
	plugin "github.com/NodeSpy/conductor/pkg/plugin"
)

func main() {
	if err := plugin.Serve(ghplugin.New()); err != nil {
		fmt.Fprintln(os.Stderr, "conductor-github:", err)
		os.Exit(1)
	}
}
