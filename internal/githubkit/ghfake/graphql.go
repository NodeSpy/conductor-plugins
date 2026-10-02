package ghfake

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// GraphQL: the operations conductor sends, parsed, validated against a
// vendored subset of GitHub's public schema (schema/graphql-subset.graphql),
// and resolved against the model. A field the schema does not have is a
// GraphQL error shaped like GitHub's (undefinedField), not a silent null.

//go:embed schema/graphql-subset.graphql
var graphqlSDL string

// ---- schema -------------------------------------------------------------------

type gqlType struct {
	kind       string // type | interface | union | enum | input | scalar
	name       string
	fields     map[string]string // field → named return type
	implements []string
	members    []string // union members
}

type gqlSchema struct{ types map[string]*gqlType }

var (
	schemaOnce sync.Once
	gschema    *gqlSchema
)

func graphSchema() *gqlSchema {
	schemaOnce.Do(func() { gschema = parseSDL(graphqlSDL) })
	return gschema
}

// parseSDL reads the vendored SDL: definitions, their fields' return types,
// implemented interfaces and union members.
func parseSDL(src string) *gqlSchema {
	s := &gqlSchema{types: map[string]*gqlType{}}
	lx := newLexer(src)
	for !lx.eof() {
		t := lx.next()
		switch t.v {
		case "type", "interface", "input":
			def := &gqlType{kind: t.v, name: lx.next().v, fields: map[string]string{}}
			if lx.peek().v == "implements" {
				lx.next()
				for lx.peek().v != "{" {
					if n := lx.next().v; n != "&" {
						def.implements = append(def.implements, n)
					}
				}
			}
			lx.expect("{")
			for lx.peek().v != "}" {
				fname := lx.next().v
				if lx.peek().v == "(" { // arguments: skip to the matching ")"
					depth := 0
					for {
						tt := lx.next()
						if tt.v == "(" {
							depth++
						} else if tt.v == ")" {
							depth--
							if depth == 0 {
								break
							}
						}
					}
				}
				lx.expect(":")
				def.fields[fname] = readTypeRef(lx)
				if lx.peek().v == "=" { // input default
					lx.next()
					lx.next()
				}
			}
			lx.expect("}")
			s.types[def.name] = def
		case "union":
			def := &gqlType{kind: "union", name: lx.next().v}
			lx.expect("=")
			for {
				if lx.peek().v == "|" {
					lx.next()
				}
				def.members = append(def.members, lx.next().v)
				if lx.peek().v != "|" {
					break
				}
			}
			s.types[def.name] = def
		case "enum":
			def := &gqlType{kind: "enum", name: lx.next().v}
			lx.expect("{")
			for lx.peek().v != "}" {
				lx.next()
			}
			lx.expect("}")
			s.types[def.name] = def
		case "scalar":
			s.types[lx.peek().v] = &gqlType{kind: "scalar", name: lx.next().v}
		}
	}
	for _, sc := range []string{"String", "Int", "Boolean", "ID", "Float", "URI", "DateTime", "GitObjectID", "HTML", "GitSSHRemote", "X509Certificate", "PreciseDateTime", "Base64String", "Date", "BigInt", "GitTimestamp"} {
		if s.types[sc] == nil {
			s.types[sc] = &gqlType{kind: "scalar", name: sc}
		}
	}
	return s
}

func readTypeRef(lx *lexer) string {
	name := ""
	for {
		t := lx.peek()
		if t.v == "[" || t.v == "]" || t.v == "!" {
			lx.next()
			continue
		}
		if name != "" {
			return name
		}
		name = lx.next().v
		for lx.peek().v == "]" || lx.peek().v == "!" {
			lx.next()
		}
		return name
	}
}

// implements reports whether object type `obj` satisfies `cond` (itself, an
// interface it implements, or a union it belongs to).
func (s *gqlSchema) satisfies(obj, cond string) bool {
	if obj == cond {
		return true
	}
	if t := s.types[obj]; t != nil {
		for _, i := range t.implements {
			if i == cond {
				return true
			}
		}
	}
	if u := s.types[cond]; u != nil && u.kind == "union" {
		for _, m := range u.members {
			if m == obj {
				return true
			}
		}
	}
	return false
}

// ---- lexer --------------------------------------------------------------------

type tok struct {
	v         string
	kind      byte // n name, s string, i number, p punct
	line, col int
}

type lexer struct {
	src       []rune
	pos       int
	line, col int
	buf       *tok
}

func newLexer(s string) *lexer { return &lexer{src: []rune(s), line: 1, col: 1} }

func (l *lexer) adv() rune {
	r := l.src[l.pos]
	l.pos++
	if r == '\n' {
		l.line++
		l.col = 1
	} else {
		l.col++
	}
	return r
}

func (l *lexer) skip() {
	for l.pos < len(l.src) {
		r := l.src[l.pos]
		if r == '#' {
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.adv()
			}
			continue
		}
		if unicode.IsSpace(r) || r == ',' || r == 0xFEFF {
			l.adv()
			continue
		}
		if strings.HasPrefix(string(l.src[l.pos:min(l.pos+3, len(l.src))]), `"""`) && l.descMode() {
			l.adv()
			l.adv()
			l.adv()
			for l.pos < len(l.src) && !strings.HasPrefix(string(l.src[l.pos:min(l.pos+3, len(l.src))]), `"""`) {
				l.adv()
			}
			for i := 0; i < 3 && l.pos < len(l.src); i++ {
				l.adv()
			}
			continue
		}
		return
	}
}

// descMode: block strings only ever appear as SDL descriptions here.
func (l *lexer) descMode() bool { return true }

func (l *lexer) eof() bool {
	if l.buf != nil {
		return false
	}
	l.skip()
	return l.pos >= len(l.src)
}

func (l *lexer) peek() tok {
	if l.buf == nil {
		t := l.lex()
		l.buf = &t
	}
	return *l.buf
}

func (l *lexer) next() tok {
	t := l.peek()
	l.buf = nil
	return t
}

func (l *lexer) expect(v string) tok {
	t := l.next()
	if t.v != v {
		panic(gqlSyntax{fmt.Sprintf("Expected %q, actual: %q", v, t.v), t.line, t.col})
	}
	return t
}

type gqlSyntax struct {
	msg       string
	line, col int
}

func (l *lexer) lex() tok {
	l.skip()
	if l.pos >= len(l.src) {
		return tok{v: "", kind: 'e', line: l.line, col: l.col}
	}
	line, col := l.line, l.col
	r := l.src[l.pos]
	switch {
	case r == '.' && strings.HasPrefix(string(l.src[l.pos:min(l.pos+3, len(l.src))]), "..."):
		l.adv()
		l.adv()
		l.adv()
		return tok{v: "...", kind: 'p', line: line, col: col}
	case strings.ContainsRune("{}():$![]=@|&", r):
		l.adv()
		return tok{v: string(r), kind: 'p', line: line, col: col}
	case r == '"':
		l.adv()
		var b strings.Builder
		for l.pos < len(l.src) && l.src[l.pos] != '"' {
			c := l.adv()
			if c == '\\' && l.pos < len(l.src) {
				e := l.adv()
				switch e {
				case 'n':
					b.WriteRune('\n')
				case 't':
					b.WriteRune('\t')
				default:
					b.WriteRune(e)
				}
				continue
			}
			b.WriteRune(c)
		}
		if l.pos < len(l.src) {
			l.adv()
		}
		return tok{v: b.String(), kind: 's', line: line, col: col}
	case r == '-' || unicode.IsDigit(r):
		start := l.pos
		l.adv()
		for l.pos < len(l.src) && (unicode.IsDigit(l.src[l.pos]) || l.src[l.pos] == '.' || l.src[l.pos] == 'e') {
			l.adv()
		}
		return tok{v: string(l.src[start:l.pos]), kind: 'i', line: line, col: col}
	case r == '_' || unicode.IsLetter(r):
		start := l.pos
		for l.pos < len(l.src) && (l.src[l.pos] == '_' || unicode.IsLetter(l.src[l.pos]) || unicode.IsDigit(l.src[l.pos])) {
			l.adv()
		}
		return tok{v: string(l.src[start:l.pos]), kind: 'n', line: line, col: col}
	}
	l.adv()
	panic(gqlSyntax{fmt.Sprintf("Unexpected character %q", r), line, col})
}

// ---- documents ----------------------------------------------------------------

type selection struct {
	alias, name string
	args        map[string]any // literal values; variables as varRef
	sel         []*selection
	on          string // inline fragment type condition ("" for a field)
	spread      string // named fragment spread
	line, col   int
}

type varRef string

type operation struct {
	kind string // query | mutation
	vars map[string]string
	sel  []*selection
}

type document struct {
	ops       []*operation
	fragments map[string]*selection // name → inline-fragment-shaped selection
}

func parseDoc(src string) (doc *document, err *gqlSyntax) {
	defer func() {
		if r := recover(); r != nil {
			if s, ok := r.(gqlSyntax); ok {
				err = &s
				return
			}
			panic(r)
		}
	}()
	lx := newLexer(src)
	doc = &document{fragments: map[string]*selection{}}
	for !lx.eof() {
		t := lx.peek()
		switch {
		case t.v == "{":
			doc.ops = append(doc.ops, &operation{kind: "query", vars: map[string]string{}, sel: parseSel(lx)})
		case t.v == "query" || t.v == "mutation":
			lx.next()
			op := &operation{kind: t.v, vars: map[string]string{}}
			if lx.peek().kind == 'n' {
				lx.next()
			}
			if lx.peek().v == "(" {
				lx.next()
				for lx.peek().v != ")" {
					lx.expect("$")
					name := lx.next().v
					lx.expect(":")
					raw := ""
					for {
						p := lx.peek()
						if p.v == "$" || p.v == ")" || p.v == "=" {
							break
						}
						raw += lx.next().v
					}
					if lx.peek().v == "=" {
						lx.next()
						parseValue(lx)
					}
					op.vars[name] = raw
				}
				lx.expect(")")
			}
			op.sel = parseSel(lx)
			doc.ops = append(doc.ops, op)
		case t.v == "fragment":
			lx.next()
			name := lx.next().v
			if lx.next().v != "on" {
				panic(gqlSyntax{"Expected \"on\"", t.line, t.col})
			}
			on := lx.next().v
			doc.fragments[name] = &selection{on: on, sel: parseSel(lx), line: t.line, col: t.col}
		default:
			panic(gqlSyntax{fmt.Sprintf("Unexpected %q", t.v), t.line, t.col})
		}
	}
	return doc, nil
}

func parseSel(lx *lexer) []*selection {
	lx.expect("{")
	var out []*selection
	for lx.peek().v != "}" {
		t := lx.next()
		if t.v == "..." {
			if lx.peek().v == "on" {
				lx.next()
				on := lx.next().v
				out = append(out, &selection{on: on, sel: parseSel(lx), line: t.line, col: t.col})
			} else {
				out = append(out, &selection{spread: lx.next().v, line: t.line, col: t.col})
			}
			continue
		}
		s := &selection{name: t.v, alias: t.v, line: t.line, col: t.col}
		if lx.peek().v == ":" {
			lx.next()
			s.name = lx.next().v
		}
		if lx.peek().v == "(" {
			lx.next()
			s.args = map[string]any{}
			for lx.peek().v != ")" {
				k := lx.next().v
				lx.expect(":")
				s.args[k] = parseValue(lx)
			}
			lx.expect(")")
		}
		for lx.peek().v == "@" { // directives: ignored
			lx.next()
			lx.next()
			if lx.peek().v == "(" {
				for lx.next().v != ")" {
				}
			}
		}
		if lx.peek().v == "{" {
			s.sel = parseSel(lx)
		}
		out = append(out, s)
	}
	lx.expect("}")
	return out
}

func parseValue(lx *lexer) any {
	t := lx.next()
	switch {
	case t.v == "$":
		return varRef(lx.next().v)
	case t.v == "[":
		var out []any
		for lx.peek().v != "]" {
			out = append(out, parseValue(lx))
		}
		lx.expect("]")
		return out
	case t.v == "{":
		out := map[string]any{}
		for lx.peek().v != "}" {
			k := lx.next().v
			lx.expect(":")
			out[k] = parseValue(lx)
		}
		lx.expect("}")
		return out
	case t.kind == 's':
		return t.v
	case t.kind == 'i':
		if n, err := strconv.ParseInt(t.v, 10, 64); err == nil {
			return float64(n)
		}
		f, _ := strconv.ParseFloat(t.v, 64)
		return f
	case t.v == "true":
		return true
	case t.v == "false":
		return false
	case t.v == "null":
		return nil
	}
	return t.v // enum value
}

// ---- validation -----------------------------------------------------------------

type gqlError map[string]any

func undefinedField(typ, field string, path []any, s *selection) gqlError {
	return gqlError{
		"path":       path,
		"extensions": map[string]any{"code": "undefinedField", "typeName": typ, "fieldName": field},
		"locations":  []any{map[string]any{"line": s.line, "column": s.col}},
		"message":    fmt.Sprintf("Field '%s' doesn't exist on type '%s'", field, typ),
	}
}

func (doc *document) validate(sel []*selection, typ string, path []any) []gqlError {
	sch := graphSchema()
	t := sch.types[typ]
	var errs []gqlError
	for _, s := range sel {
		switch {
		case s.spread != "":
			fr := doc.fragments[s.spread]
			if fr == nil {
				errs = append(errs, gqlError{"message": fmt.Sprintf("Fragment %s was used, but not defined", s.spread)})
				continue
			}
			errs = append(errs, doc.validate(fr.sel, fr.on, path)...)
		case s.on != "":
			if sch.types[s.on] == nil {
				errs = append(errs, gqlError{"message": fmt.Sprintf("No such type %s, so it can't be a fragment condition", s.on),
					"extensions": map[string]any{"code": "undefinedType", "typeName": s.on}})
				continue
			}
			errs = append(errs, doc.validate(s.sel, s.on, path)...)
		case s.name == "__typename":
		default:
			p := append(append([]any(nil), path...), s.alias)
			if t == nil || (t.kind != "type" && t.kind != "interface") {
				errs = append(errs, undefinedField(typ, s.name, p, s))
				continue
			}
			rt, ok := t.fields[s.name]
			if !ok {
				errs = append(errs, undefinedField(typ, s.name, p, s))
				continue
			}
			if len(s.sel) > 0 {
				errs = append(errs, doc.validate(s.sel, rt, p)...)
			}
		}
	}
	return errs
}

// ---- execution ------------------------------------------------------------------

// gqlNode is a resolvable object: its concrete type, and a field resolver.
type gqlNode interface {
	typename() string
	field(name string, args map[string]any) (any, error)
}

type gqlNotFound struct{ msg string }

func (e gqlNotFound) Error() string { return e.msg }

type exec struct {
	f    *Fake
	doc  *document
	vars map[string]any
	who  *identity
	errs []gqlError
}

func (x *exec) resolveArgs(args map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = x.value(v)
	}
	return out
}

func (x *exec) value(v any) any {
	switch t := v.(type) {
	case varRef:
		return x.vars[string(t)]
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = x.value(e)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range t {
			out[k] = x.value(e)
		}
		return out
	}
	return v
}

func (x *exec) run(n gqlNode, sel []*selection, path []any) map[string]any {
	out := map[string]any{}
	sch := graphSchema()
	for _, s := range sel {
		switch {
		case s.spread != "":
			fr := x.doc.fragments[s.spread]
			if fr != nil && sch.satisfies(n.typename(), fr.on) {
				merge(out, x.run(n, fr.sel, path))
			}
		case s.on != "":
			if sch.satisfies(n.typename(), s.on) {
				merge(out, x.run(n, s.sel, path))
			}
		case s.name == "__typename":
			out[s.alias] = n.typename()
		default:
			p := append(append([]any(nil), path...), s.alias)
			v, err := n.field(s.name, x.resolveArgs(s.args))
			if err != nil {
				e := gqlError{"path": p, "locations": []any{map[string]any{"line": s.line, "column": s.col}}, "message": err.Error()}
				if _, nf := err.(gqlNotFound); nf {
					e["type"] = "NOT_FOUND"
				} else if strings.HasPrefix(err.Error(), "FORBIDDEN") {
					e["type"] = "FORBIDDEN"
					e["message"] = strings.TrimPrefix(err.Error(), "FORBIDDEN: ")
				}
				x.errs = append(x.errs, e)
				out[s.alias] = nil
				continue
			}
			out[s.alias] = x.shape(v, s, p)
		}
	}
	return out
}

func (x *exec) shape(v any, s *selection, path []any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case gqlNode:
		if t == nil {
			return nil
		}
		return x.run(t, s.sel, path)
	case []gqlNode:
		out := make([]any, 0, len(t))
		for i, e := range t {
			out = append(out, x.run(e, s.sel, append(append([]any(nil), path...), i)))
		}
		return out
	}
	return v
}

func (f *Fake) serveGraphQL(w http.ResponseWriter, r *http.Request, raw []byte) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, 400, map[string]any{"message": "Problems parsing JSON", "documentation_url": "https://docs.github.com/graphql"})
		return
	}
	h := r.Header.Get("Authorization")
	tok := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(h, "token "), "Bearer "))
	f.mu.Lock()
	who := f.tokens[tok]
	if who != nil {
		f.usage[tok]++
	}
	f.mu.Unlock()
	if who == nil {
		writeJSON(w, 401, map[string]any{"message": "Bad credentials", "documentation_url": "https://docs.github.com/graphql"})
		return
	}
	w.Header().Set("X-RateLimit-Resource", "graphql")
	doc, serr := parseDoc(req.Query)
	if serr != nil {
		writeJSON(w, 200, map[string]any{"errors": []any{map[string]any{"message": "Parse error on " + serr.msg,
			"locations": []any{map[string]any{"line": serr.line, "column": serr.col}}}}})
		return
	}
	if len(doc.ops) != 1 {
		writeJSON(w, 200, map[string]any{"errors": []any{map[string]any{"message": "An operation name is required"}}})
		return
	}
	op := doc.ops[0]
	root := "Query"
	if op.kind == "mutation" {
		root = "Mutation"
	}
	if errs := doc.validate(op.sel, root, []any{op.kind}); len(errs) > 0 {
		writeJSON(w, 200, map[string]any{"errors": errs})
		return
	}
	for name, typ := range op.vars {
		if strings.HasSuffix(typ, "!") && req.Variables[name] == nil {
			writeJSON(w, 200, map[string]any{"errors": []any{map[string]any{
				"extensions": map[string]any{"value": nil, "problems": []any{map[string]any{"path": []any{}, "explanation": "Expected value to not be null"}}},
				"locations":  []any{map[string]any{"line": 1, "column": 1}},
				"message":    fmt.Sprintf("Variable $%s of type %s was provided invalid value", name, typ)}}})
			return
		}
	}
	f.mu.Lock()
	x := &exec{f: f, doc: doc, vars: req.Variables, who: who}
	data := x.run(&gqlRoot{x: x, mutation: op.kind == "mutation"}, op.sel, nil)
	f.mu.Unlock()
	out := map[string]any{"data": data}
	if len(x.errs) > 0 {
		out["errors"] = x.errs
	}
	writeJSON(w, 200, out)
}

// ---- resolvers ------------------------------------------------------------------

type gqlRoot struct {
	x        *exec
	mutation bool
}

func (q *gqlRoot) typename() string {
	if q.mutation {
		return "Mutation"
	}
	return "Query"
}

func first(args map[string]any) int {
	if n, ok := args["first"].(float64); ok {
		return int(n)
	}
	return 100
}

func (q *gqlRoot) field(name string, args map[string]any) (any, error) {
	f := q.x.f
	switch name {
	case "repository":
		owner, _ := args["owner"].(string)
		n, _ := args["name"].(string)
		r := f.repo(owner + "/" + n)
		if r == nil || (q.x.who.kind == "installation" && r.InstallationID != q.x.who.inst) {
			return nil, gqlNotFound{fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", owner, n)}
		}
		return &gRepo{f, r}, nil
	case "node":
		id, _ := args["id"].(string)
		if n := f.nodeByID(id); n != nil {
			return n, nil
		}
		return nil, gqlNotFound{fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id)}
	case "viewer":
		return &gUser{f, q.x.who.user}, nil
	case "addReaction", "removeReaction":
		in, _ := args["input"].(map[string]any)
		subj, _ := in["subjectId"].(string)
		content, _ := in["content"].(string)
		rs := f.reactionsOf(subj)
		if rs == nil {
			return nil, gqlNotFound{fmt.Sprintf("Could not resolve to a node with the global id of '%s'", subj)}
		}
		rest := gqlReactionContent[content]
		if rest == "" {
			return nil, fmt.Errorf("Argument 'content' on InputObject '%s' has an invalid value (%s). Expected type 'ReactionContent!'.", map[bool]string{true: "AddReactionInput", false: "RemoveReactionInput"}[name == "addReaction"], content)
		}
		if name == "addReaction" {
			f.react(rs, q.x.who.user, rest)
		} else {
			kept := (*rs)[:0]
			for _, x := range *rs {
				if !(lower(x.User.Login) == lower(q.x.who.user.Login) && x.Content == rest) {
					kept = append(kept, x)
				}
			}
			*rs = kept
		}
		return &gObj{"AddReactionPayload", map[string]any{"reaction": &gObj{"Reaction", map[string]any{"content": content}}}}, nil
	case "markPullRequestReadyForReview", "convertPullRequestToDraft":
		in, _ := args["input"].(map[string]any)
		id, _ := in["pullRequestId"].(string)
		gp, ok := f.nodeByID(id).(*gPull)
		if !ok {
			return nil, gqlNotFound{fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id)}
		}
		ready := name == "markPullRequestReadyForReview"
		if gp.p.Draft == ready {
			gp.p.Draft = !ready
			gp.p.touched()
			action := "ready_for_review"
			if !ready {
				action = "converted_to_draft"
			}
			f.emitPull(gp.r, gp.p, action, q.x.who.user, nil)
		}
		return &gObj{"MarkPullRequestReadyForReviewPayload", map[string]any{"clientMutationId": nil, "pullRequest": gp}}, nil
	}
	return nil, fmt.Errorf("ghfake: Query.%s is not resolved", name)
}

var gqlReactionContent = map[string]string{
	"THUMBS_UP": "+1", "THUMBS_DOWN": "-1", "LAUGH": "laugh", "HOORAY": "hooray",
	"CONFUSED": "confused", "HEART": "heart", "ROCKET": "rocket", "EYES": "eyes",
}

// gObj is a plain resolved object.
type gObj struct {
	t string
	m map[string]any
}

func (o *gObj) typename() string { return o.t }
func (o *gObj) field(name string, _ map[string]any) (any, error) {
	if v, ok := o.m[name]; ok {
		return v, nil
	}
	return nil, nil
}

func conn(t string, nodes []gqlNode, total int) gqlNode {
	return &gObj{t, map[string]any{"nodes": nodes, "totalCount": total}}
}

func limit(ns []gqlNode, n int) []gqlNode {
	if n >= 0 && len(ns) > n {
		return ns[:n]
	}
	return ns
}

type gUser struct {
	f *Fake
	u *User
}

func (g *gUser) typename() string {
	switch g.u.Type {
	case "Bot":
		return "Bot"
	case "Organization":
		return "Organization"
	}
	return "User"
}

func (g *gUser) field(name string, _ map[string]any) (any, error) {
	switch name {
	case "login":
		if g.u.Type == "Bot" {
			return strings.TrimSuffix(g.u.Login, "[bot]"), nil // GraphQL drops the [bot] suffix
		}
		return g.u.Login, nil
	case "id":
		return nodeID(nodePrefixUser(g.u), g.u.ID), nil
	case "databaseId":
		return g.u.ID, nil
	case "url":
		return htmlBase + "/" + g.u.Login, nil
	}
	return nil, nil
}

func actor(f *Fake, u *User) gqlNode {
	if u == nil {
		return nil
	}
	return &gUser{f, u}
}

type gRepo struct {
	f *Fake
	r *Repo
}

func (g *gRepo) typename() string { return "Repository" }
func (g *gRepo) field(name string, args map[string]any) (any, error) {
	switch name {
	case "nameWithOwner":
		return g.r.FullName(), nil
	case "name":
		return g.r.Name, nil
	case "id":
		return nodeID("R", g.r.ID), nil
	case "pullRequest":
		n := int(args["number"].(float64))
		if p := g.r.pull(n); p != nil {
			return &gPull{g.f, g.r, p}, nil
		}
		return nil, gqlNotFound{fmt.Sprintf("Could not resolve to a PullRequest with the number of %d.", n)}
	case "issue":
		n := int(args["number"].(float64))
		if is := g.r.Issues[n]; is != nil && is.Pull == nil {
			return &gIssue{g.f, g.r, is}, nil
		}
		return nil, gqlNotFound{fmt.Sprintf("Could not resolve to an Issue with the number of %d.", n)}
	}
	return nil, nil
}

type gPull struct {
	f *Fake
	r *Repo
	p *Pull
}

func (g *gPull) typename() string { return "PullRequest" }
func (g *gPull) field(name string, args map[string]any) (any, error) {
	p, f := g.p, g.f
	switch name {
	case "id":
		return nodeID("PR", p.ID), nil
	case "number":
		return p.Issue.Number, nil
	case "title":
		return p.Issue.Title, nil
	case "headRefOid":
		return p.HeadSHA, nil
	case "headRefName":
		return p.HeadRef, nil
	case "baseRefName":
		return p.BaseRef, nil
	case "isDraft":
		return p.Draft, nil
	case "state":
		if p.Merged {
			return "MERGED", nil
		}
		return strings.ToUpper(p.Issue.State), nil
	case "mergeStateStatus":
		_, st := f.mergeState(g.r, p)
		return strings.ToUpper(st), nil
	case "reviewDecision":
		if d := f.reviewDecision(g.r, p); d != "" {
			return d, nil
		}
		return nil, nil
	case "author":
		return actor(f, p.Issue.User), nil
	case "labels":
		var ns []gqlNode
		for _, l := range p.Issue.Labels {
			ns = append(ns, &gObj{"Label", map[string]any{"name": l}})
		}
		return conn("LabelConnection", limit(ns, first(args)), len(ns)), nil
	case "reviewThreads":
		var ns []gqlNode
		for _, th := range p.Threads {
			ns = append(ns, &gThread{f, g.r, p, th})
		}
		return conn("PullRequestReviewThreadConnection", limit(ns, first(args)), len(ns)), nil
	case "reviews":
		states := map[string]bool{}
		for _, s := range strsOf(args["states"]) {
			states[s] = true
		}
		var ns []gqlNode
		for _, rv := range p.Reviews {
			if rv.State == "PENDING" || (len(states) > 0 && !states[rv.State]) {
				continue
			}
			ns = append(ns, &gReview{f, rv})
		}
		return conn("PullRequestReviewConnection", limit(ns, first(args)), len(ns)), nil
	case "latestOpinionatedReviews":
		var ns []gqlNode
		for _, rv := range p.Reviews { // in submission order, latest per author
			if latest := p.latestReviews()[lower(rv.User.Login)]; latest == rv {
				ns = append(ns, &gReview{f, rv})
			}
		}
		return conn("PullRequestReviewConnection", limit(ns, first(args)), len(ns)), nil
	}
	return nil, nil
}

func strsOf(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, e := range l {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

type gReview struct {
	f  *Fake
	rv *Review
}

func (g *gReview) typename() string { return "PullRequestReview" }
func (g *gReview) field(name string, _ map[string]any) (any, error) {
	switch name {
	case "state":
		return g.rv.State, nil
	case "author":
		return actor(g.f, g.rv.User), nil
	case "id":
		return nodeID("PRR", g.rv.ID), nil
	case "databaseId":
		return g.rv.ID, nil
	case "body":
		return g.rv.Body, nil
	}
	return nil, nil
}

type gThread struct {
	f  *Fake
	r  *Repo
	p  *Pull
	th *Thread
}

func (g *gThread) typename() string { return "PullRequestReviewThread" }
func (g *gThread) field(name string, args map[string]any) (any, error) {
	switch name {
	case "id":
		return nodeID("PRRT", g.th.ID), nil
	case "isResolved":
		return g.th.Resolved, nil
	case "isOutdated":
		return g.th.Outdated, nil
	case "path":
		return g.th.Comments[0].Path, nil
	case "comments":
		var ns []gqlNode
		for _, cm := range g.th.Comments {
			ns = append(ns, &gComment{g.f, g.r, g.p, cm})
		}
		return conn("PullRequestReviewCommentConnection", limit(ns, first(args)), len(ns)), nil
	}
	return nil, nil
}

type gComment struct {
	f  *Fake
	r  *Repo
	p  *Pull
	cm *ReviewComment
}

func (g *gComment) typename() string { return "PullRequestReviewComment" }
func (g *gComment) field(name string, _ map[string]any) (any, error) {
	cm := g.cm
	switch name {
	case "databaseId":
		return cm.ID, nil
	case "id":
		return nodeID("PRRC", cm.ID), nil
	case "author":
		return actor(g.f, cm.User), nil
	case "path":
		return cm.Path, nil
	case "line":
		if cm.Thread != nil && cm.Thread.Outdated {
			return nil, nil
		}
		return cm.Line, nil
	case "originalLine":
		return cm.OriginalLine, nil
	case "body":
		return cm.Body, nil
	case "url":
		return fmt.Sprintf("%s/%s/pull/%d#discussion_r%d", htmlBase, g.r.FullName(), g.p.Issue.Number, cm.ID), nil
	}
	return nil, nil
}

type gIssue struct {
	f  *Fake
	r  *Repo
	is *Issue
}

func (g *gIssue) typename() string { return "Issue" }
func (g *gIssue) field(name string, args map[string]any) (any, error) {
	is, f := g.is, g.f
	switch name {
	case "number":
		return is.Number, nil
	case "title":
		return is.Title, nil
	case "id":
		return nodeID("I", is.ID), nil
	case "author":
		return actor(f, is.User), nil
	case "repository":
		return &gRepo{f, g.r}, nil
	case "labels":
		var ns []gqlNode
		for _, l := range is.Labels {
			ns = append(ns, &gObj{"Label", map[string]any{"name": l}})
		}
		return conn("LabelConnection", limit(ns, first(args)), len(ns)), nil
	case "assignees":
		var ns []gqlNode
		for _, a := range is.Assignees {
			if u := f.user(a); u != nil {
				ns = append(ns, &gUser{f, u})
			}
		}
		return conn("UserConnection", limit(ns, first(args)), len(ns)), nil
	case "linkedBranches":
		return conn("LinkedBranchConnection", nil, len(is.LinkedBranches)), nil
	case "closedByPullRequestsReferences":
		return conn("PullRequestConnection", nil, len(is.ClosedBy)), nil
	case "projectItems":
		var ns []gqlNode
		if len(is.ProjectFields) > 0 {
			ns = append(ns, &gItem{f, g.r, is})
		}
		return conn("ProjectV2ItemConnection", limit(ns, first(args)), len(ns)), nil
	}
	return nil, nil
}

// gItem is an issue's Projects v2 item; its single-select field values are
// Issue.ProjectFields.
type gItem struct {
	f  *Fake
	r  *Repo
	is *Issue
}

func (g *gItem) typename() string { return "ProjectV2Item" }
func (g *gItem) field(name string, args map[string]any) (any, error) {
	switch name {
	case "id":
		return nodeID("PVTI", g.is.ID), nil
	case "content":
		return &gIssue{g.f, g.r, g.is}, nil
	case "fieldValueByName":
		n, _ := args["name"].(string)
		if v, ok := g.is.ProjectFields[n]; ok {
			return fieldValue(n, v), nil
		}
		return nil, nil
	case "fieldValues":
		var ns []gqlNode
		for _, k := range sortedKeys(g.is.ProjectFields) {
			ns = append(ns, fieldValue(k, g.is.ProjectFields[k]))
		}
		return conn("ProjectV2ItemFieldValueConnection", limit(ns, first(args)), len(ns)), nil
	}
	return nil, nil
}

func fieldValue(field, value string) gqlNode {
	return &gObj{"ProjectV2ItemFieldSingleSelectValue", map[string]any{
		"name": value, "field": &gObj{"ProjectV2SingleSelectField", map[string]any{"name": field}}}}
}

// nodeByID resolves a global node id.
func (f *Fake) nodeByID(id string) gqlNode {
	prefix, num, ok := parseNodeID(id)
	if !ok {
		return nil
	}
	for _, r := range f.repos {
		for _, is := range r.Issues {
			switch prefix {
			case "PR":
				if is.Pull != nil && is.Pull.ID == num {
					return &gPull{f, r, is.Pull}
				}
			case "I":
				if is.ID == num {
					return &gIssue{f, r, is}
				}
			case "PVTI":
				if is.ID == num && len(is.ProjectFields) > 0 {
					return &gItem{f, r, is}
				}
			case "PRR":
				if is.Pull != nil {
					for _, rv := range is.Pull.Reviews {
						if rv.ID == num {
							return &gReview{f, rv}
						}
					}
				}
			}
		}
	}
	return nil
}

// reactionsOf finds the reactions list of a reactable node: a review, an
// issue comment, or a review comment.
func (f *Fake) reactionsOf(id string) *[]*Reaction {
	prefix, num, ok := parseNodeID(id)
	if !ok {
		return nil
	}
	for _, r := range f.repos {
		for _, is := range r.Issues {
			if prefix == "IC" {
				for _, cm := range is.Comments {
					if cm.ID == num {
						return &cm.Reactions
					}
				}
			}
			if is.Pull == nil {
				continue
			}
			if prefix == "PRR" {
				for _, rv := range is.Pull.Reviews {
					if rv.ID == num {
						return &rv.Reactions
					}
				}
			}
			if prefix == "PRRC" {
				for _, cm := range is.Pull.ReviewComments {
					if cm.ID == num {
						return &cm.Reactions
					}
				}
			}
		}
	}
	return nil
}
