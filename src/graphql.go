package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// --- GraphQL document parser (the subset the suite's 18 documents use) ----

// gqlValue is one parsed GraphQL value literal.
type gqlValue struct {
	kind    byte // 's' string, 'n' number, 'b' bool, 'z' null, 'e' enum, 'l' list, 'o' object, 'v' variable
	str     string
	num     float64
	b       bool
	list    []*gqlValue
	obj     map[string]*gqlValue
	varName string
}

// gqlField is one parsed field (or inline fragment) in a selection set.
type gqlField struct {
	alias    string
	name     string
	args     map[string]*gqlValue
	sel      []gqlField
	fragment bool   // an inline fragment "... on Type { ... }"
	fragType string // the type condition, "" when absent
}

// gqlOp is one parsed operation.
type gqlOp struct {
	kind string // query | mutation
	sel  []gqlField
}

// gqlParser is a recursive-descent parser over the query text.
type gqlParser struct {
	src string
	pos int
}

func (p *gqlParser) errf(format string, args ...any) error {
	return fmt.Errorf("graphql parse error at %d: %s", p.pos, fmt.Sprintf(format, args...))
}

// skipWs skips whitespace, commas and comments.
func (p *gqlParser) skipWs() {
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.pos++
			continue
		}
		if c == '#' {
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
			continue
		}
		return
	}
}

func (p *gqlParser) peek() byte {
	p.skipWs()
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *gqlParser) expect(c byte) error {
	p.skipWs()
	if p.pos >= len(p.src) || p.src[p.pos] != c {
		return p.errf("expected %q", string(c))
	}
	p.pos++
	return nil
}

// readName reads a GraphQL name ([_A-Za-z][_0-9A-Za-z]*).
func (p *gqlParser) readName() (string, error) {
	p.skipWs()
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			p.pos++
			continue
		}
		break
	}
	if p.pos == start {
		return "", p.errf("expected a name")
	}
	return p.src[start:p.pos], nil
}

// readString reads a "..." string with escapes.
func (p *gqlParser) readString() (string, error) {
	p.skipWs()
	if p.pos >= len(p.src) || p.src[p.pos] != '"' {
		return "", p.errf("expected a string")
	}
	p.pos++
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		p.pos++
		switch c {
		case '"':
			return b.String(), nil
		case '\\':
			if p.pos >= len(p.src) {
				return "", p.errf("unterminated escape")
			}
			e := p.src[p.pos]
			p.pos++
			switch e {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'u':
				if p.pos+4 > len(p.src) {
					return "", p.errf("bad unicode escape")
				}
				n, err := strconv.ParseInt(p.src[p.pos:p.pos+4], 16, 32)
				if err != nil {
					return "", p.errf("bad unicode escape")
				}
				p.pos += 4
				b.WriteRune(rune(n))
			default:
				return "", p.errf("bad escape %q", string(e))
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", p.errf("unterminated string")
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	p.skipWs()
	if p.pos >= len(p.src) {
		return nil, p.errf("expected a value")
	}
	c := p.src[p.pos]
	switch {
	case c == '"':
		s, err := p.readString()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: 's', str: s}, nil
	case c == '[':
		p.pos++
		v := &gqlValue{kind: 'l'}
		for {
			if p.peek() == ']' {
				p.pos++
				return v, nil
			}
			item, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.list = append(v.list, item)
		}
	case c == '{':
		p.pos++
		v := &gqlValue{kind: 'o', obj: map[string]*gqlValue{}}
		for {
			if p.peek() == '}' {
				p.pos++
				return v, nil
			}
			key, err := p.readName()
			if err != nil {
				return nil, err
			}
			if err := p.expect(':'); err != nil {
				return nil, err
			}
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			v.obj[key] = val
		}
	case c == '$':
		p.pos++
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: 'v', varName: name}, nil
	case c == '-' || c >= '0' && c <= '9':
		start := p.pos
		for p.pos < len(p.src) {
			c := p.src[p.pos]
			if c >= '0' && c <= '9' || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
				p.pos++
				continue
			}
			break
		}
		n, err := strconv.ParseFloat(p.src[start:p.pos], 64)
		if err != nil {
			return nil, p.errf("bad number")
		}
		return &gqlValue{kind: 'n', num: n}, nil
	default:
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		switch name {
		case "true":
			return &gqlValue{kind: 'b', b: true}, nil
		case "false":
			return &gqlValue{kind: 'b', b: false}, nil
		case "null":
			return &gqlValue{kind: 'z'}, nil
		default:
			return &gqlValue{kind: 'e', str: name}, nil
		}
	}
}

// parseArguments parses "(name: value, ...)".
func (p *gqlParser) parseArguments() (map[string]*gqlValue, error) {
	args := map[string]*gqlValue{}
	if err := p.expect('('); err != nil {
		return nil, err
	}
	for {
		if p.peek() == ')' {
			p.pos++
			return args, nil
		}
		name, err := p.readName()
		if err != nil {
			return nil, err
		}
		if err := p.expect(':'); err != nil {
			return nil, err
		}
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name] = val
	}
}

// skipDirectives skips "@name(args)" occurrences.
func (p *gqlParser) skipDirectives() error {
	for p.peek() == '@' {
		p.pos++
		if _, err := p.readName(); err != nil {
			return err
		}
		if p.peek() == '(' {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

// skipVarDefs skips "( $var: Type = default, ... )".
func (p *gqlParser) skipVarDefs() error {
	if p.peek() != '(' {
		return nil
	}
	p.pos++
	depth := 1
	for p.pos < len(p.src) && depth > 0 {
		switch p.src[p.pos] {
		case '(':
			depth++
		case ')':
			depth--
		case '"':
			if _, err := p.readString(); err != nil {
				return err
			}
			continue
		}
		p.pos++
	}
	if depth != 0 {
		return p.errf("unterminated variable definitions")
	}
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]gqlField, error) {
	if err := p.expect('{'); err != nil {
		return nil, err
	}
	var fields []gqlField
	for {
		if p.peek() == '}' {
			p.pos++
			return fields, nil
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
	}
}

func (p *gqlParser) parseField() (gqlField, error) {
	p.skipWs()
	// Inline fragment or fragment spread.
	if strings.HasPrefix(p.src[p.pos:], "...") {
		p.pos += 3
		f := gqlField{fragment: true}
		name, err := p.readName()
		if err != nil {
			return f, err
		}
		if name == "on" {
			typ, err := p.readName()
			if err != nil {
				return f, err
			}
			f.fragType = typ
		} else {
			// A named fragment spread: the suite never uses one; ignore it.
			return f, nil
		}
		if err := p.skipDirectives(); err != nil {
			return f, err
		}
		sel, err := p.parseSelectionSet()
		if err != nil {
			return f, err
		}
		f.sel = sel
		return f, nil
	}
	name, err := p.readName()
	if err != nil {
		return gqlField{}, err
	}
	f := gqlField{name: name, alias: name}
	if p.peek() == ':' {
		p.pos++
		actual, err := p.readName()
		if err != nil {
			return f, err
		}
		f.alias = name
		f.name = actual
	}
	if p.peek() == '(' {
		args, err := p.parseArguments()
		if err != nil {
			return f, err
		}
		f.args = args
	}
	if err := p.skipDirectives(); err != nil {
		return f, err
	}
	if p.peek() == '{' {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return f, err
		}
		f.sel = sel
	}
	return f, nil
}

func parseDocument(src string) ([]gqlOp, error) {
	p := &gqlParser{src: src}
	var ops []gqlOp
	for {
		if p.peek() == 0 {
			break
		}
		if p.peek() == '{' {
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			ops = append(ops, gqlOp{kind: "query", sel: sel})
			continue
		}
		kw, err := p.readName()
		if err != nil {
			return nil, err
		}
		switch kw {
		case "query", "mutation":
			op := gqlOp{kind: kw}
			// Optional operation name.
			if c := p.peek(); c != '(' && c != '{' && c != '@' {
				if _, err := p.readName(); err != nil {
					return nil, err
				}
			}
			if err := p.skipVarDefs(); err != nil {
				return nil, err
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			op.sel = sel
			ops = append(ops, op)
		case "fragment":
			// Fragment definitions are parsed and discarded (unused by the
			// suite's documents).
			if _, err := p.readName(); err != nil {
				return nil, err
			}
			if _, err := p.readName(); err != nil { // "on"
				return nil, err
			}
			if _, err := p.readName(); err != nil { // the type
				return nil, err
			}
			if _, err := p.parseSelectionSet(); err != nil {
				return nil, err
			}
		default:
			return nil, p.errf("unexpected %q", kw)
		}
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("graphql parse error: empty document")
	}
	return ops, nil
}

// --- value resolution -----------------------------------------------------

// gqlCtx carries execution state: the request variables and the error list.
type gqlCtx struct {
	s    *Server
	vars map[string]any
	errs []map[string]any
}

func (c *gqlCtx) addErr(format string, args ...any) {
	c.errs = append(c.errs, map[string]any{"message": fmt.Sprintf(format, args...)})
}

// resolveValue turns a parsed literal (or variable) into a plain Go value.
func (c *gqlCtx) resolveValue(v *gqlValue) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case 's':
		return v.str
	case 'n':
		return v.num
	case 'b':
		return v.b
	case 'z':
		return nil
	case 'e':
		return v.str
	case 'v':
		if c.vars != nil {
			return c.vars[v.varName]
		}
		return nil
	case 'l':
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, c.resolveValue(item))
		}
		return out
	case 'o':
		out := make(map[string]any, len(v.obj))
		for k, item := range v.obj {
			out[k] = c.resolveValue(item)
		}
		return out
	}
	return nil
}

func (c *gqlCtx) arg(f gqlField, name string) any {
	if f.args == nil {
		return nil
	}
	return c.resolveValue(f.args[name])
}

func argString(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func argInt(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return def
}

func argStringList(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// --- executor -------------------------------------------------------------

// gqlNode is a resolved value with its GraphQL type.
type gqlNode struct {
	typ string
	v   any // *Repo, *Issue, *Pull, *Comment, string, int64, bool, *issueConn, ...
}

// gqlNull marks an explicit GraphQL null: v == nil alone is not null, because
// the viewer/owner User nodes carry no Go value.
var gqlNull = new(struct{})

// nullNode is an explicit GraphQL null of the given type.
func nullNode(typ string) gqlNode { return gqlNode{typ: typ, v: gqlNull} }

// isScalarType reports whether the type resolves to a JSON scalar directly.
func isScalarType(typ string) bool {
	switch typ {
	case "String", "Int", "Boolean", "ID":
		return true
	}
	return false
}

// executeSelection resolves a selection set against a parent value.
func (c *gqlCtx) executeSelection(parent gqlNode, sel []gqlField) map[string]any {
	out := map[string]any{}
	for _, f := range sel {
		if f.fragment {
			if f.fragType == "" || f.fragType == parent.typ {
				sub := c.executeSelection(parent, f.sel)
				for k, v := range sub {
					out[k] = v
				}
			}
			continue
		}
		child, ok := c.resolveField(parent, f)
		if !ok {
			c.addErr("Cannot query field %q on type %q.", f.name, parent.typ)
			continue
		}
		out[f.alias] = c.materialize(child, f.sel)
	}
	return out
}

// materialize turns a resolved node into JSON, recursing into selections.
func (c *gqlCtx) materialize(n gqlNode, sel []gqlField) any {
	if n.v == gqlNull {
		return nil
	}
	if isScalarType(n.typ) {
		return n.v
	}
	if n.typ == "List" {
		items := n.v.([]gqlNode)
		out := make([]any, 0, len(items))
		for _, item := range items {
			out = append(out, c.materialize(item, sel))
		}
		return out
	}
	return c.executeSelection(n, sel)
}

// --- connections ----------------------------------------------------------

type issueConn struct {
	issues   []*Issue
	hasNext  bool
	endCursor string
}

type prConn struct {
	pulls    []*Pull
	hasNext  bool
	endCursor string
}

type fileConn struct {
	paths []string
}

// cursorFor encodes a creation-sequence cursor.
func cursorFor(seq int64) string {
	return encodeGlobalID("cursor", strconv.FormatInt(seq, 10))
}

func cursorSeq(cursor string) (int64, bool) {
	kind, parts, ok := decodeGlobalID(cursor)
	if !ok || kind != "cursor" || len(parts) != 1 {
		return 0, false
	}
	n, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// issuesConnection builds an IssueConnection for a repo: newest first,
// filtered by state, paged by first/after.
func issuesConnection(repo *Repo, states []string, first int, after string) *issueConn {
	keep := map[string]bool{}
	for _, st := range states {
		keep[st] = true
	}
	afterSeq, hasAfter := int64(0), false
	if after != "" {
		afterSeq, hasAfter = cursorSeq(after)
	}
	// Newest first (sequence descending).
	sorted := make([]*Issue, 0, len(repo.IssueOrder))
	for i := len(repo.IssueOrder) - 1; i >= 0; i-- {
		is := repo.IssueOrder[i]
		if len(keep) > 0 && !keep[issueStateName(is)] {
			continue
		}
		if hasAfter && is.Seq >= afterSeq {
			continue
		}
		sorted = append(sorted, is)
	}
	if first <= 0 {
		first = len(sorted)
	}
	hasNext := len(sorted) > first
	page := sorted
	if len(page) > first {
		page = page[:first]
	}
	endCursor := ""
	if len(page) > 0 {
		endCursor = cursorFor(page[len(page)-1].Seq)
	}
	return &issueConn{issues: page, hasNext: hasNext, endCursor: endCursor}
}

func prsConnection(repo *Repo, first int) *prConn {
	sorted := make([]*Pull, 0, len(repo.PullOrder))
	for i := len(repo.PullOrder) - 1; i >= 0; i-- {
		sorted = append(sorted, repo.PullOrder[i])
	}
	if first <= 0 {
		first = len(sorted)
	}
	hasNext := len(sorted) > first
	page := sorted
	if len(page) > first {
		page = page[:first]
	}
	endCursor := ""
	if len(page) > 0 {
		endCursor = cursorFor(page[len(page)-1].Seq)
	}
	return &prConn{pulls: page, hasNext: hasNext, endCursor: endCursor}
}

// --- field resolution -----------------------------------------------------

// resolveField resolves one field on a parent node. ok=false means the field
// does not exist on the parent type (an execution error, not a null).
func (c *gqlCtx) resolveField(parent gqlNode, f gqlField) (gqlNode, bool) {
	switch parent.typ {
	case "Query":
		return c.resolveRoot(f)
	case "Mutation":
		return c.resolveMutation(f)
	case "User":
		switch f.name {
		case "login":
			return gqlNode{"String", viewerLogin}, true
		case "id":
			return gqlNode{"ID", encodeGlobalID("User", strconv.Itoa(viewerID))}, true
		case "name":
			return gqlNode{"String", viewerName}, true
		}
	case "Repository":
		return c.resolveRepository(parent.v.(*Repo), f)
	case "Issue":
		return c.resolveIssue(parent.v.(*Issue), f)
	case "PullRequest":
		return c.resolvePullRequest(parent.v.(*Pull), parent, f)
	case "Ref":
		if f.name == "name" {
			return gqlNode{"String", parent.v.(string)}, true
		}
	case "IssueConnection":
		conn := parent.v.(*issueConn)
		switch f.name {
		case "nodes":
			nodes := make([]gqlNode, 0, len(conn.issues))
			for _, is := range conn.issues {
				nodes = append(nodes, gqlNode{"Issue", is})
			}
			return gqlNode{"List", nodes}, true
		case "pageInfo":
			return gqlNode{"PageInfo", conn}, true
		case "totalCount":
			return gqlNode{"Int", len(conn.issues)}, true
		}
	case "PullRequestConnection":
		conn := parent.v.(*prConn)
		switch f.name {
		case "nodes":
			nodes := make([]gqlNode, 0, len(conn.pulls))
			for _, pr := range conn.pulls {
				nodes = append(nodes, gqlNode{"PullRequest", pr})
			}
			return gqlNode{"List", nodes}, true
		case "pageInfo":
			return gqlNode{"PageInfo", conn}, true
		case "totalCount":
			return gqlNode{"Int", len(conn.pulls)}, true
		}
	case "PullRequestFileConnection":
		conn := parent.v.(*fileConn)
		switch f.name {
		case "nodes":
			nodes := make([]gqlNode, 0, len(conn.paths))
			for _, p := range conn.paths {
				nodes = append(nodes, gqlNode{"PullRequestFile", p})
			}
			return gqlNode{"List", nodes}, true
		case "pageInfo":
			return gqlNode{"PageInfo", conn}, true
		}
	case "PullRequestFile":
		if f.name == "path" {
			return gqlNode{"String", parent.v.(string)}, true
		}
	case "PageInfo":
		switch f.name {
		case "hasNextPage":
			switch conn := parent.v.(type) {
			case *issueConn:
				return gqlNode{"Boolean", conn.hasNext}, true
			case *prConn:
				return gqlNode{"Boolean", conn.hasNext}, true
			case *fileConn:
				return gqlNode{"Boolean", false}, true
			}
		case "endCursor":
			switch conn := parent.v.(type) {
			case *issueConn:
				if conn.endCursor == "" {
					return nullNode("String"), true
				}
				return gqlNode{"String", conn.endCursor}, true
			case *prConn:
				if conn.endCursor == "" {
					return nullNode("String"), true
				}
				return gqlNode{"String", conn.endCursor}, true
			case *fileConn:
				return nullNode("String"), true
			}
		case "hasPreviousPage":
			return gqlNode{"Boolean", false}, true
		case "startCursor":
			return nullNode("String"), true
		}
	case "CreateIssuePayload":
		if f.name == "issue" {
			return gqlNode{"Issue", parent.v.(*Issue)}, true
		}
	case "CloseIssuePayload":
		if f.name == "issue" {
			return gqlNode{"Issue", parent.v.(*Issue)}, true
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if f.name == "pullRequest" {
			return gqlNode{"PullRequest", parent.v.(*Pull)}, true
		}
	case "AddCommentPayload":
		if f.name == "commentEdge" {
			return gqlNode{"IssueCommentEdge", parent.v.(*Comment)}, true
		}
	case "IssueCommentEdge":
		if f.name == "node" {
			return gqlNode{"IssueComment", parent.v.(*Comment)}, true
		}
	case "IssueComment":
		cmt := parent.v.(*Comment)
		switch f.name {
		case "id":
			// The comment's global id needs its repo and issue; the parent
			// carries the comment only, so encode by comment id alone.
			return gqlNode{"ID", encodeGlobalID("IssueComment", strconv.FormatInt(cmt.ID, 10))}, true
		case "body":
			return gqlNode{"String", cmt.Body}, true
		}
	case "__Type":
		return c.resolveType(parent.v.(*gqlTypeInfo), f)
	case "__Field", "__EnumValue":
		return resolveIntrospectionLeaf(parent, f)
	}
	return gqlNode{}, false
}

func (c *gqlCtx) resolveRoot(f gqlField) (gqlNode, bool) {
	switch f.name {
	case "viewer":
		return gqlNode{"User", nil}, true
	case "repository":
		owner := argString(c.arg(f, "owner"), "")
		name := argString(c.arg(f, "name"), "")
		repo := c.s.getRepo(owner, name)
		if repo == nil {
			return nullNode("Repository"), true
		}
		return gqlNode{"Repository", repo}, true
	case "node":
		id := argString(c.arg(f, "id"), "")
		kind, parts, ok := decodeGlobalID(id)
		if !ok {
			return nullNode("Node"), true
		}
		switch kind {
		case "Repository":
			if len(parts) == 1 {
				if rid, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
					if repo := c.s.findRepoByID(rid); repo != nil {
						return gqlNode{"Repository", repo}, true
					}
				}
			}
		case "Issue":
			if len(parts) == 2 {
				rid, _ := strconv.ParseInt(parts[0], 10, 64)
				num, _ := strconv.Atoi(parts[1])
				if repo := c.s.findRepoByID(rid); repo != nil {
					if is := repo.Issues[num]; is != nil {
						return gqlNode{"Issue", is}, true
					}
				}
			}
		case "PullRequest":
			if len(parts) == 2 {
				rid, _ := strconv.ParseInt(parts[0], 10, 64)
				num, _ := strconv.Atoi(parts[1])
				if repo := c.s.findRepoByID(rid); repo != nil {
					if pr := repo.Pulls[num]; pr != nil {
						return gqlNode{"PullRequest", pr}, true
					}
				}
			}
		}
		return nullNode("Node"), true
	case "__type":
		name := argString(c.arg(f, "name"), "")
		info := typeInfo(name)
		if info == nil {
			return nullNode("__Type"), true
		}
		return gqlNode{"__Type", info}, true
	}
	return gqlNode{}, false
}

func (c *gqlCtx) resolveRepository(repo *Repo, f gqlField) (gqlNode, bool) {
	switch f.name {
	case "id":
		return gqlNode{"ID", repoGlobalID(repo.ID)}, true
	case "databaseId":
		return gqlNode{"Int", repo.ID}, true
	case "name":
		return gqlNode{"String", repo.Name}, true
	case "nameWithOwner":
		return gqlNode{"String", repo.Owner + "/" + repo.Name}, true
	case "owner":
		return gqlNode{"User", nil}, true
	case "defaultBranchRef":
		if _, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; !ok {
			return nullNode("Ref"), true
		}
		return gqlNode{"Ref", repo.DefaultBranch}, true
	case "viewerPermission":
		return gqlNode{"String", "ADMIN"}, true
	case "hasIssuesEnabled":
		return gqlNode{"Boolean", true}, true
	case "issue":
		number := argInt(c.arg(f, "number"), 0)
		if is := repo.Issues[number]; is != nil {
			return gqlNode{"Issue", is}, true
		}
		return nullNode("Issue"), true
	case "issues":
		first := argInt(c.arg(f, "first"), 0)
		after := argString(c.arg(f, "after"), "")
		states := argStringList(c.arg(f, "states"))
		return gqlNode{"IssueConnection", issuesConnection(repo, states, first, after)}, true
	case "pullRequest":
		number := argInt(c.arg(f, "number"), 0)
		if pr := repo.Pulls[number]; pr != nil {
			return gqlNode{"PullRequest", pr}, true
		}
		return nullNode("PullRequest"), true
	case "pullRequests":
		first := argInt(c.arg(f, "first"), 0)
		return gqlNode{"PullRequestConnection", prsConnection(repo, first)}, true
	}
	return gqlNode{}, false
}

func (c *gqlCtx) resolveIssue(issue *Issue, f gqlField) (gqlNode, bool) {
	// The issue's repo id is needed for its global id; find it by scan.
	repoID := func() int64 {
		for _, r := range c.s.repoList {
			if r.Issues[issue.Number] == issue {
				return r.ID
			}
		}
		return 0
	}
	switch f.name {
	case "id":
		return gqlNode{"ID", issueGlobalID(repoID(), issue.Number)}, true
	case "number":
		return gqlNode{"Int", issue.Number}, true
	case "title":
		return gqlNode{"String", issue.Title}, true
	case "state":
		return gqlNode{"String", issueStateName(issue)}, true
	case "body":
		return gqlNode{"String", issue.Body}, true
	}
	return gqlNode{}, false
}

func (c *gqlCtx) resolvePullRequest(pr *Pull, parent gqlNode, f gqlField) (gqlNode, bool) {
	// Find the PR's repo (needed for ids and file diffs).
	repo := func() *Repo {
		for _, r := range c.s.repoList {
			if r.Pulls[pr.Number] == pr {
				return r
			}
		}
		return nil
	}
	switch f.name {
	case "id":
		r := repo()
		var rid int64
		if r != nil {
			rid = r.ID
		}
		return gqlNode{"ID", pullGlobalID(rid, pr.Number)}, true
	case "number":
		return gqlNode{"Int", pr.Number}, true
	case "title":
		return gqlNode{"String", pr.Title}, true
	case "body":
		return gqlNode{"String", pr.Body}, true
	case "headRefName":
		return gqlNode{"String", pr.HeadRef}, true
	case "baseRefName":
		return gqlNode{"String", pr.BaseRef}, true
	case "mergeable":
		r := repo()
		if r == nil {
			return gqlNode{"String", "UNKNOWN"}, true
		}
		return gqlNode{"String", pullMergeable(r, pr)}, true
	case "state":
		return gqlNode{"String", pullStateName(pr)}, true
	case "merged":
		return gqlNode{"Boolean", pr.Merged}, true
	case "files":
		r := repo()
		if r == nil {
			return gqlNode{"PullRequestFileConnection", &fileConn{}}, true
		}
		first := argInt(c.arg(f, "first"), 0)
		baseTree, headTree, ok := pullTrees(r, pr)
		paths := []string{}
		if ok {
			for _, ch := range diffTrees(r, baseTree, headTree) {
				paths = append(paths, ch.Path)
			}
		}
		if first > 0 && len(paths) > first {
			paths = paths[:first]
		}
		return gqlNode{"PullRequestFileConnection", &fileConn{paths: paths}}, true
	}
	return gqlNode{}, false
}

// --- mutations ------------------------------------------------------------

// inputMap returns an argument as a resolved input object.
func (c *gqlCtx) inputMap(f gqlField, name string) map[string]any {
	if m, ok := c.arg(f, name).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func (c *gqlCtx) resolveMutation(f gqlField) (gqlNode, bool) {
	switch f.name {
	case "createIssue":
		input := c.inputMap(f, "input")
		repo := c.s.findRepoByID(repoIDFromGlobalID(argString(input["repositoryId"], "")))
		if repo == nil {
			c.addErr("repository not found")
			return nullNode("CreateIssuePayload"), true
		}
		issue := c.s.createIssueInternal(repo, argString(input["title"], ""), argString(input["body"], ""), nil)
		return gqlNode{"CreateIssuePayload", issue}, true
	case "closeIssue":
		input := c.inputMap(f, "input")
		issue := c.issueFromGlobalID(argString(input["issueId"], ""))
		if issue == nil {
			c.addErr("issue not found")
			return nullNode("CloseIssuePayload"), true
		}
		closeIssueInternal(issue)
		return gqlNode{"CloseIssuePayload", issue}, true
	case "createPullRequest":
		input := c.inputMap(f, "input")
		repo := c.s.findRepoByID(repoIDFromGlobalID(argString(input["repositoryId"], "")))
		if repo == nil {
			c.addErr("repository not found")
			return nullNode("CreatePullRequestPayload"), true
		}
		pr, errMsg := c.s.createPullInternal(repo, argString(input["title"], ""),
			argString(input["body"], ""), argString(input["headRefName"], ""), argString(input["baseRefName"], ""))
		if errMsg != "" {
			c.addErr("%s", errMsg)
			return nullNode("CreatePullRequestPayload"), true
		}
		return gqlNode{"CreatePullRequestPayload", pr}, true
	case "mergePullRequest":
		input := c.inputMap(f, "input")
		pr, repo := c.pullFromGlobalID(argString(input["pullRequestId"], ""))
		if pr == nil || repo == nil {
			c.addErr("pull request not found")
			return nullNode("MergePullRequestPayload"), true
		}
		if _, status, msg := c.s.mergePull(repo, pr); status != 0 {
			c.addErr("%s", msg)
			return nullNode("MergePullRequestPayload"), true
		}
		return gqlNode{"MergePullRequestPayload", pr}, true
	case "addComment":
		input := c.inputMap(f, "input")
		issue := c.issueFromGlobalID(argString(input["subjectId"], ""))
		if issue == nil {
			c.addErr("subject not found")
			return nullNode("AddCommentPayload"), true
		}
		comment := c.s.addCommentInternal(issue, argString(input["body"], ""))
		return gqlNode{"AddCommentPayload", comment}, true
	}
	return gqlNode{}, false
}

func repoIDFromGlobalID(id string) int64 {
	kind, parts, ok := decodeGlobalID(id)
	if !ok || kind != "Repository" || len(parts) != 1 {
		return 0
	}
	rid, _ := strconv.ParseInt(parts[0], 10, 64)
	return rid
}

func (c *gqlCtx) issueFromGlobalID(id string) *Issue {
	kind, parts, ok := decodeGlobalID(id)
	if !ok || kind != "Issue" || len(parts) != 2 {
		return nil
	}
	rid, _ := strconv.ParseInt(parts[0], 10, 64)
	num, _ := strconv.Atoi(parts[1])
	if repo := c.s.findRepoByID(rid); repo != nil {
		return repo.Issues[num]
	}
	return nil
}

func (c *gqlCtx) pullFromGlobalID(id string) (*Pull, *Repo) {
	kind, parts, ok := decodeGlobalID(id)
	if !ok || kind != "PullRequest" || len(parts) != 2 {
		return nil, nil
	}
	rid, _ := strconv.ParseInt(parts[0], 10, 64)
	num, _ := strconv.Atoi(parts[1])
	if repo := c.s.findRepoByID(rid); repo != nil {
		return repo.Pulls[num], repo
	}
	return nil, nil
}

// --- introspection --------------------------------------------------------

// gqlTypeInfo is the introspection data for one named type.
type gqlTypeInfo struct {
	name       string
	kind       string
	fields     []string
	enumValues []string
}

func typeInfo(name string) *gqlTypeInfo {
	switch name {
	case "Issue":
		return &gqlTypeInfo{name: "Issue", kind: "OBJECT", fields: []string{
			"id", "number", "title", "state", "body", "author", "createdAt",
			"updatedAt", "labels", "comments", "url", "repository",
		}}
	case "PullRequest":
		return &gqlTypeInfo{name: "PullRequest", kind: "OBJECT", fields: []string{
			"id", "number", "headRefName", "baseRefName", "mergeable", "state",
			"merged", "files", "title", "body", "author", "createdAt",
			"updatedAt", "url", "repository",
		}}
	case "Repository":
		return &gqlTypeInfo{name: "Repository", kind: "OBJECT", fields: []string{
			"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef",
			"viewerPermission", "hasIssuesEnabled", "issue", "issues",
			"pullRequests", "pullRequest", "url", "description", "createdAt",
		}}
	case "SearchType":
		return &gqlTypeInfo{name: "SearchType", kind: "ENUM", enumValues: []string{
			"ISSUE", "REPOSITORY", "USER", "DISCUSSION",
		}}
	case "User":
		return &gqlTypeInfo{name: "User", kind: "OBJECT", fields: []string{
			"id", "login", "name", "email", "url",
		}}
	}
	return nil
}

func (c *gqlCtx) resolveType(info *gqlTypeInfo, f gqlField) (gqlNode, bool) {
	switch f.name {
	case "name":
		return gqlNode{"String", info.name}, true
	case "kind":
		return gqlNode{"String", info.kind}, true
	case "fields":
		nodes := make([]gqlNode, 0, len(info.fields))
		for _, name := range info.fields {
			nodes = append(nodes, gqlNode{"__Field", name})
		}
		return gqlNode{"List", nodes}, true
	case "enumValues":
		nodes := make([]gqlNode, 0, len(info.enumValues))
		for _, name := range info.enumValues {
			nodes = append(nodes, gqlNode{"__EnumValue", name})
		}
		return gqlNode{"List", nodes}, true
	}
	// __Field / __EnumValue name resolution is wired into resolveField.
	return gqlNode{}, false
}

// resolveIntrospectionLeaf resolves the "name" field of __Field/__EnumValue.
func resolveIntrospectionLeaf(parent gqlNode, f gqlField) (gqlNode, bool) {
	if f.name == "name" {
		if s, ok := parent.v.(string); ok {
			return gqlNode{"String", s}, true
		}
	}
	return gqlNode{}, false
}

// --- HTTP handler ---------------------------------------------------------

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.writeErr(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if r.Method != "POST" {
		s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := readBody(r, 16<<20)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "bad request")
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var reqs []gqlRequest
		if err := json.Unmarshal(trimmed, &reqs); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{
				"errors": []map[string]any{{"message": "malformed request"}},
			})
			return
		}
		resps := make([]any, len(reqs))
		for i := range reqs {
			resps[i] = s.execGraphQL(&reqs[i])
		}
		s.writeJSON(w, http.StatusOK, resps)
		return
	}
	var req gqlRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"errors": []map[string]any{{"message": "malformed request"}},
		})
		return
	}
	s.writeJSON(w, http.StatusOK, s.execGraphQL(&req))
}

func (s *Server) execGraphQL(req *gqlRequest) map[string]any {
	ops, err := parseDocument(req.Query)
	if err != nil {
		return map[string]any{
			"data":   nil,
			"errors": []map[string]any{{"message": err.Error()}},
		}
	}
	op := ops[0]
	rootType := "Query"
	if op.kind == "mutation" {
		rootType = "Mutation"
		s.mu.Lock()
		defer s.mu.Unlock()
	} else {
		s.mu.RLock()
		defer s.mu.RUnlock()
	}
	ctx := &gqlCtx{s: s, vars: req.Variables}
	data := ctx.executeSelection(gqlNode{typ: rootType}, op.sel)
	resp := map[string]any{"data": data}
	if len(ctx.errs) > 0 {
		resp["errors"] = ctx.errs
	}
	return resp
}