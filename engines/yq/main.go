// Command conductor-yq is the `use: yq` STEP ENGINE as an external conductor
// plugin: mikefarah/yq's own library (yqlib) driving a yq expression over the
// step's inputs — the YAML counterpart to engines/jq's gojq.
//
// The contract is the same data-in, data-out shape as jq: the YAML INPUT
// document is req.Inputs (the step's rendered ctx, marshaled to YAML text),
// req.Code is a yq expression (mikefarah/yq syntax: ".items | map(.name)",
// ".a.b = \"x\"", "(.. | select(tag == \"!!str\")) |= sub(\"foo\",\"bar\")",
// …), and the expression's result becomes the step's outputs. There is no
// ctx.store/ctx.sql/ctx.memory face here, for the same reason jq has none: yq
// has no notion of a host call, and a data-plane step reaches it through a
// neighboring step instead — this engine only reshapes what it is given.
//
// OUTPUT CONTRACT differs from jq in one way, because YAML is not JSON: a yq
// expression can match zero, one, or many result nodes (".items[]" exploding
// an array, same as jq), and those nodes are worth keeping around as rendered
// YAML text, verbatim, in addition to their Go-value form — that text is
// yq's whole edge over jq, since it preserves comments, anchors and key order
// that a round-trip through Go values would drop. So this engine hands back
// BOTH shapes:
//
//   - each matched node is decoded into a Go value and the collected list is
//     run through enginekit's single-value rule: zero nodes -> no outputs
//     ({}); one node -> enginekit.WrapValue(that value) (a mapping becomes
//     named outputs, anything else becomes value:); more than one node ->
//     enginekit.WrapValue(the values, as a list), landing under value: — the
//     same way jq's own multi-result queries do.
//   - whenever there was at least one matched node, an additional yaml:
//     output carries yq's own rendered text for the whole result (in one
//     mapping's case, "a: 1\nb: 2\n"; for several matched nodes, whatever yq
//     itself would print for that expression — see the execYQ/renderYAML
//     comments for why that is NOT simply "each value, "---"-joined"), so a
//     downstream step can write it to a file or hand it to another
//     YAML-aware tool without conductor's JSON round-trip lossifying it. (If
//     a yq mapping result happens to use "yaml" as one of its own keys, this
//     output wins — same as any other merge, last write takes the field.)
//
// SANDBOX: unlike gojq, yq's expression language has operators that reach
// outside the value it was handed — env(...)/strenv(...) read the plugin
// process's environment, and load(...)/load_str(...) read arbitrary files
// off whatever filesystem the plugin runs on. yqlib gates exactly these
// behind yqlib.ConfiguredSecurityPreferences, a package-level switch, and
// this engine turns both off in init() before serving a single request:
// DisableEnvOps and DisableFileOps. A step's `code:` calling env(), strenv(),
// load() or load_str() gets yq's own "operations have been disabled" error
// rather than a live read. The remaining reach-outside operator, system(...)
// (shelling out to run a command), is gated by a separate switch,
// EnableSystemOps, which yqlib itself defaults to false; this engine leaves
// it at that default rather than ever setting it true. With all three off,
// the declared Capabilities are empty, same as jq: no egress, no fs, no
// commands, no spawns.
//
// stdout is the RPC transport; all logging goes to stderr, and yqlib's own
// stderr logger (log/slog under pkg/yqlib.GetLogger()) is turned down to
// warnings-and-above in init() so a step's expression doesn't fill the
// plugin's stderr with yq's debug trace.
package main

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/mikefarah/yq/v4/pkg/yqlib"
	yaml "go.yaml.in/yaml/v3"

	plugin "github.com/NodeSpy/conductor/pkg/plugin"

	"github.com/NodeSpy/conductor-plugins/internal/enginekit"
)

func init() {
	// See the SANDBOX note above: a plugin process's environment and
	// filesystem are not step data, so the two yq operators that would read
	// them are turned off before this engine serves a single request.
	// system(...) is left at yqlib's own default (disabled) rather than ever
	// being turned on.
	yqlib.ConfiguredSecurityPreferences.DisableEnvOps = true
	yqlib.ConfiguredSecurityPreferences.DisableFileOps = true

	yqlib.GetLogger().SetLevel(slog.LevelWarn)
}

func describe() plugin.Decl {
	return plugin.Decl{
		Kind: plugin.KindStep,
		ABI:  plugin.EngineABI,
		Type: "yq",
		Desc: "yq code steps on mikefarah/yq's yqlib: the step inputs, " +
			"rendered as YAML, are the yq expression's input document, and " +
			"the expression's result becomes the step's outputs — one " +
			"mapping result becomes named outputs, one scalar/sequence " +
			"result becomes value:, several documents collect into a " +
			"value: list, and no documents at all is no outputs. An " +
			"additional yaml: output always carries the raw rendered YAML " +
			"text, preserving comments/anchors/ordering that a Go-value " +
			"round-trip would drop. env/strenv/load/load_str are disabled; " +
			"system is left at yq's own disabled default.",
		Capabilities: plugin.Capabilities{},
	}
}

func run(ctx context.Context, req plugin.RunRequest, host *plugin.Host) (plugin.RunResult, error) {
	fmt.Fprintf(os.Stderr, "conductor-yq: run instance=%s inputs=%d\n", req.Instance, len(req.Inputs))
	outputs, err := execYQ(ctx, req.Code, req.Inputs)
	if err != nil {
		return plugin.RunResult{}, err
	}
	return plugin.RunResult{Outputs: outputs}, nil
}

// execYQ renders the step's inputs as YAML, runs one yq expression over
// them, and maps the matched result node(s) back onto the step's output
// contract.
//
// This drives yqlib's lower-level pieces (ReadDocuments + AllAtOnceEvaluator)
// rather than its StringEvaluator convenience wrapper, because the two
// questions this engine has to answer — "how many results came out?" and
// "what does the combined rendered text look like?" — are NOT the same
// question here. yq's printer only inserts a "---" document separator
// between results that trace back to distinct INPUT documents (see
// yqlib.Printer); several results exploded out of the same input document
// (".items[]" over a three-element array, say) print back to back with no
// separator at all, which is exactly how the real yq CLI behaves — and which
// makes the rendered text ambiguous to split back into "how many results"
// after the fact (three bare scalar lines like "a\nb\nc\n" is itself one
// valid YAML plain scalar). Keeping the actual matched-node list lets this
// engine count results the way it counted them, and encode each one to YAML
// individually — an unambiguous single-value document every time — for the
// Go-value side of the contract, while still handing the SAME node list to
// yq's own printer once for the combined yaml: text.
func execYQ(ctx context.Context, code string, inputs map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	inputYAML, err := marshalInputs(inputs)
	if err != nil {
		return nil, fmt.Errorf("yq: marshal inputs: %w", err)
	}

	prefs := yqlib.NewDefaultYamlPreferences()
	encoder := yqlib.NewYamlEncoder(prefs)
	decoder := yqlib.NewYamlDecoder(prefs)

	// yqlib's evaluators have no context-aware variant — evaluation walks
	// the expression to completion or not at all, the same limitation the jq
	// engine would have if gojq's RunWithContext didn't exist. Running it on
	// its own goroutine and racing the result against ctx.Done() at least
	// gives the step's own timeout/cancellation somewhere to land: a
	// cancelled step returns immediately with ctx.Err() rather than blocking
	// on a runaway expression (an infinite `until`, say) until the plugin
	// process itself is killed. The goroutine is left to finish on its own;
	// its result is simply discarded into a buffered channel.
	type evalResult struct {
		matches *list.List
		err     error
	}
	resultCh := make(chan evalResult, 1)
	go func() {
		documents, err := yqlib.ReadDocuments(strings.NewReader(inputYAML), decoder)
		if err != nil {
			resultCh <- evalResult{err: fmt.Errorf("yq: read input: %w", err)}
			return
		}
		matches, err := yqlib.NewAllAtOnceEvaluator().EvaluateCandidateNodes(code, documents)
		if err != nil {
			resultCh <- evalResult{err: fmt.Errorf("yq: %w", err)}
			return
		}
		resultCh <- evalResult{matches: matches}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-resultCh:
		if res.err != nil {
			return nil, res.err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return buildOutputs(res.matches, encoder)
	}
}

// buildOutputs applies both halves of this engine's output contract to one
// expression's matched result nodes: enginekit's single-value rule over each
// node decoded to its own Go value, plus the combined rendered YAML text
// (yq's own printer, run once over the same node list) whenever there was at
// least one result.
func buildOutputs(matches *list.List, encoder yqlib.Encoder) (map[string]any, error) {
	if matches.Len() == 0 {
		return map[string]any{}, nil
	}

	docs := make([]any, 0, matches.Len())
	for el := matches.Front(); el != nil; el = el.Next() {
		node, ok := el.Value.(*yqlib.CandidateNode)
		if !ok {
			return nil, fmt.Errorf("yq: unexpected result node type %T", el.Value)
		}
		v, err := decodeNode(node, encoder)
		if err != nil {
			return nil, fmt.Errorf("yq: decode result: %w", err)
		}
		docs = append(docs, v)
	}

	rendered, err := renderYAML(matches, encoder)
	if err != nil {
		return nil, fmt.Errorf("yq: render result: %w", err)
	}

	var parsed any
	if len(docs) == 1 {
		parsed = docs[0]
	} else {
		parsed = docs
	}

	outputs := enginekit.WrapValue(parsed)
	outputs["yaml"] = rendered
	return outputs, nil
}

// decodeNode encodes a single result node to YAML in isolation — one
// self-contained document, never ambiguous the way several bare scalars
// concatenated together can be — and decodes that text back into a plain Go
// value for the WrapValue side of the contract.
func decodeNode(node *yqlib.CandidateNode, encoder yqlib.Encoder) (any, error) {
	var buf bytes.Buffer
	if err := encoder.Encode(&buf, node); err != nil {
		return nil, err
	}
	var v any
	if err := yaml.Unmarshal(buf.Bytes(), &v); err != nil {
		return nil, err
	}
	return v, nil
}

// renderYAML runs yq's own printer over the matched nodes once, producing
// the exact text the `yq` CLI itself would print for this expression —
// document separators included, or omitted, by yq's own rules — for the
// yaml: output.
func renderYAML(matches *list.List, encoder yqlib.Encoder) (string, error) {
	var buf bytes.Buffer
	printer := yqlib.NewPrinter(encoder, yqlib.NewSinglePrinterWriter(&buf))
	if err := printer.PrintResults(matches); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// marshalInputs renders the step's ctx as YAML text, the document yq's
// evaluator reads as its input. A nil map (a step with no inputs) still
// renders as a valid, empty YAML mapping ("{}\n").
func marshalInputs(inputs map[string]any) (string, error) {
	if inputs == nil {
		inputs = map[string]any{}
	}
	b, err := yaml.Marshal(inputs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func main() {
	if err := plugin.Serve(plugin.EngineFunc(describe, run)); err != nil {
		fmt.Fprintf(os.Stderr, "conductor-yq: serve: %v\n", err)
		os.Exit(1)
	}
}
