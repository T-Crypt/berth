// Package wire edits a llama-swap config so every model in its roster takes a
// berth before it loads its weights. llama-swap starts a model on the first
// request that names it, so the only place a lease can be waited for is the
// model's own cmd: the wrapper line runs the original command through
// `berth run`, which holds it until the card is free and hands it over when
// another engine asks. One hand-wrapped line per model is how a roster ends up
// with four of twenty-two models wired, and why an unwired model's whole-card
// load dies against whatever else holds the card; this package does the whole
// roster in one pass and stays re-runnable as the roster changes.
//
// The edit is line-based and surgical: comments, key order, formatting and
// every unrelated line stay byte-for-byte. The roster and the result are
// checked against a YAML parse, never against the line walker alone.
package wire

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Options are the wrapper's flags and the edit's rules.
type Options struct {
	Bin      string   // the berth binary the wrapper line runs
	Group    string   // lease group every model counts for; "" leaves --group out
	Priority string   // batch, interactive or foreground; "" leaves --priority out
	APIBase  string   // llama-swap base URL; the unload endpoint hangs off it
	Exclude  []string // model names to leave alone (path.Match patterns)
	Update   bool     // rewrite existing wrapper lines that carry other flags
}

// Validate rejects an option set that would write a broken or misleading line.
func (o Options) Validate() error {
	if o.Bin == "" {
		return errors.New("bin: empty; pass --bin with the berth binary path")
	}
	switch o.Priority {
	case "", "batch", "interactive", "foreground":
	default:
		return fmt.Errorf("priority %q is not batch, interactive or foreground", o.Priority)
	}
	u, err := url.Parse(o.APIBase)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("api %q must be an http(s)://host:port base URL", o.APIBase)
	}
	for _, pat := range o.Exclude {
		if _, err := path.Match(pat, "x"); err != nil {
			return fmt.Errorf("exclude pattern %q: %w", pat, err)
		}
	}
	return nil
}

// Model is one roster entry and what the wire did, or would do, to it.
type Model struct {
	Name   string `json:"name" yaml:"name"`
	Action string `json:"action" yaml:"action"` // wired | add | update | skip
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
	Line   string `json:"line,omitempty" yaml:"line,omitempty"` // the wrapper, for add and update
}

// Result is one run over one file.
type Result struct {
	Path    string  `json:"path" yaml:"path"`
	Models  []Model `json:"models" yaml:"models"`
	Changed bool    `json:"changed" yaml:"changed"` // any add or update

	content string // the edited file; NewContent reads it
}

// NewContent returns the edited file, unchanged when nothing was added or
// updated.
func (r *Result) NewContent() string { return r.content }

// Count counts models with one action.
func (r *Result) Count(action string) int {
	n := 0
	for _, m := range r.Models {
		if m.Action == action {
			n++
		}
	}
	return n
}

// LlamaSwap plans the edit of one llama-swap config: every model's cmd gets a
// `berth run` wrapper line unless the model is excluded, proxied or written in
// a form the walker refuses to touch (a single-line cmd, which a person should
// reformat first). Nothing is written here; Write applies the result.
func LlamaSwap(pathArg string, o Options) (*Result, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(pathArg)
	if err != nil {
		return nil, err
	}
	text := string(raw)
	names, err := roster(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", pathArg, err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s: no models: key with entries; is this a llama-swap config?", pathArg)
	}

	lines, trailingNL := splitLines(text)
	rows, blockEnd, err := modelKeyRows(lines, len(names))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", pathArg, err)
	}

	// Every plan reads the original lines; the edits are applied afterwards,
	// bottom-up, so one model's insert never moves another model's target.
	r := &Result{Path: pathArg, Models: make([]Model, 0, len(names))}
	wrapped := map[string]string{} // model -> the fragment its cmd must carry
	var plans []editPlan
	for i, name := range names {
		end := blockEnd
		if i+1 < len(rows) {
			end = rows[i+1]
		}
		m, plan := planModel(lines, rows[i], end, name, o)
		if plan != nil {
			plans = append(plans, *plan)
		}
		switch m.Action {
		case "wired", "add", "update":
			wrapped[name] = "run --as " + shellWord(name) + " "
		}
		r.Models = append(r.Models, m)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].at > plans[j].at })
	for _, p := range plans {
		if p.replace {
			lines[p.at] = p.line
			continue
		}
		out := make([]string, 0, len(lines)+1)
		out = append(out, lines[:p.at]...)
		out = append(out, p.line)
		out = append(out, lines[p.at:]...)
		lines = out
	}
	r.Changed = r.Count("add")+r.Count("update") > 0
	r.content = joinLines(lines, trailingNL)

	// Verify against a parse, plan or not: the walker's output is the
	// product, and a silent formatting surprise is the one failure worth
	// refusing outright.
	if err := verify(r.content, names, wrapped); err != nil {
		return nil, fmt.Errorf("%s: refusing to return an edit that does not verify: %w", pathArg, err)
	}
	return r, nil
}

// Write applies a changed result: the original file is kept beside it with a
// timestamped .bak-wire name, and the new content lands via a temp file and a
// rename, so a crash never leaves a half-written config. llama-swap reloads a
// watched config on the write; nothing is signalled from here.
func Write(r *Result) (backup string, err error) {
	if !r.Changed {
		return "", nil
	}
	fi, err := os.Stat(r.Path)
	if err != nil {
		return "", err
	}
	old, err := os.ReadFile(r.Path)
	if err != nil {
		return "", err
	}
	backup = fmt.Sprintf("%s.bak-wire-%s", r.Path, time.Now().Format("20060102T150405"))
	if err := os.WriteFile(backup, old, fi.Mode().Perm()); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	tmp := r.Path + ".wire-tmp"
	if err := os.WriteFile(tmp, []byte(r.content), fi.Mode().Perm()); err != nil {
		return backup, err
	}
	if err := os.Rename(tmp, r.Path); err != nil {
		os.Remove(tmp)
		return backup, err
	}
	return backup, nil
}

// editPlan is one planned change to the original lines: insert a line before
// index at, or replace the line at index at.
type editPlan struct {
	at      int
	replace bool
	line    string
}

// planModel decides one model's action and its edit, if any. keyRow opens the
// model; end is the next model's key row, or the end of the models: block.
// All indexes are into the original lines: the caller applies every plan
// after all of them are made, so one insert cannot move another model.
func planModel(lines []string, keyRow, end int, name string, o Options) (Model, *editPlan) {
	indent := leadingSpaces(lines[keyRow])

	for _, pat := range o.Exclude {
		if ok, _ := path.Match(pat, name); ok {
			return Model{Name: name, Action: "skip", Reason: "excluded by pattern " + pat}, nil
		}
	}

	cmdRow, cmdIndent := -1, 0
	for i := keyRow + 1; i < end; i++ {
		if ind := leadingSpaces(lines[i]); ind > indent && strings.HasPrefix(strings.TrimLeft(lines[i], " "), "cmd:") {
			cmdRow, cmdIndent = i, ind
			break
		}
	}
	if cmdRow < 0 {
		return Model{Name: name, Action: "skip", Reason: "no cmd: the model is proxied or started externally"}, nil
	}

	// Only block scalars (cmd: >, cmd: |, cmd: >- ...) are touched: a
	// single-line cmd would need its whole value rewritten, and guessing at
	// its quoting is how a config gets corrupted. Reformat and re-run wire.
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(lines[cmdRow], " "), "cmd:"))
	style := rest
	if i := strings.IndexAny(style, " \t#"); i >= 0 {
		style = style[:i]
	}
	switch style {
	case ">", ">-", ">+", "|", "|-", "|+":
	case "":
		return Model{Name: name, Action: "skip", Reason: "empty cmd"}, nil
	default:
		return Model{Name: name, Action: "skip", Reason: "cmd is a single-line scalar; make it a > block and wire again"}, nil
	}

	// The block's content: lines deeper than cmd:, blank lines included,
	// until a non-blank line at cmd:'s indent or above ends it.
	contentStart, contentEnd := -1, end
	for i := cmdRow + 1; i < end; i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if leadingSpaces(lines[i]) <= cmdIndent {
			contentEnd = i
			break
		}
		if contentStart < 0 {
			contentStart = i
		}
	}

	wrapRow, wrapIndent := -1, 0
	for i := cmdRow + 1; i < contentEnd; i++ {
		if wrappedAs(lines[i]) == name {
			wrapRow, wrapIndent = i, leadingSpaces(lines[i])
			break
		}
	}

	if wrapRow >= 0 {
		if !o.Update {
			return Model{Name: name, Action: "wired", Reason: "already runs under berth run"}, nil
		}
		desired := wrapperLine(name, o, wrapIndent, lineEnding(lines[wrapRow]))
		if lines[wrapRow] == desired {
			return Model{Name: name, Action: "wired", Reason: "already runs under berth run with these flags"}, nil
		}
		return Model{Name: name, Action: "update", Line: desired}, &editPlan{at: wrapRow, replace: true, line: desired}
	}

	at := cmdRow + 1
	if contentStart >= 0 {
		at = contentStart
	}
	insertIndent := cmdIndent + 2
	if contentStart >= 0 {
		insertIndent = leadingSpaces(lines[contentStart])
	}
	ending := "\n"
	if at < len(lines) {
		ending = lineEnding(lines[at])
	}
	line := wrapperLine(name, o, insertIndent, ending)
	return Model{Name: name, Action: "add", Line: line}, &editPlan{at: at, line: line}
}

// wrapperLine is the full line inserted into the block: berth run, the flags,
// and the -- that ends berth's own flag list. The model's original cmd
// follows on the next line.
func wrapperLine(name string, o Options, indent int, ending string) string {
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", indent))
	b.WriteString(shellWord(o.Bin))
	b.WriteString(" run --as ")
	b.WriteString(shellWord(name))
	if o.Group != "" {
		b.WriteString(" --group ")
		b.WriteString(shellWord(o.Group))
	}
	if o.Priority != "" {
		b.WriteString(" --priority ")
		b.WriteString(shellWord(o.Priority))
	}
	b.WriteString(" --drain-post ")
	b.WriteString(shellWord(strings.TrimRight(o.APIBase, "/") + "/api/models/unload/" + url.PathEscape(name)))
	b.WriteString(" --")
	b.WriteString(ending)
	return b.String()
}

// wrappedAs names the model a line wraps in `berth run --as NAME`, or "". The
// line is read as shell words, so any berth path, any flag order and a quoted
// name all count.
func wrappedAs(line string) string {
	fields := shellFields(line)
	for i := 1; i+2 < len(fields); i++ {
		if fields[i] != "run" || fields[i+1] != "--as" {
			continue
		}
		base := fields[i-1]
		if j := strings.LastIndexByte(base, '/'); j >= 0 {
			base = base[j+1:]
		}
		if base != "berth" {
			continue
		}
		return fields[i+2]
	}
	return ""
}

// shellFields splits a line into shell words: whitespace outside quotes
// separates, single and double quotes group, and a backslash escapes the next
// character outside single quotes. It is deliberately small: it only reads
// wrapper lines this package and its operators write.
func shellFields(line string) []string {
	var fields []string
	var cur strings.Builder
	open := byte(0)
	escaped := false
	flush := func() {
		if cur.Len() > 0 || open != 0 {
			fields = append(fields, cur.String())
		}
		cur.Reset()
	}
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case escaped:
			cur.WriteByte(ch)
			escaped = false
		case ch == '\\' && open != '\'':
			escaped = true
		case open != 0:
			if ch == open {
				if open == '\'' && i+1 < len(line) && line[i+1] == '\'' {
					cur.WriteByte('\'') // '' inside '...' is one quote
					i++
					continue
				}
				open = 0
				continue
			}
			cur.WriteByte(ch)
		case ch == '\'' || ch == '"':
			open = ch
		case ch == ' ' || ch == '\t':
			flush()
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return fields
}

// shellWord quotes a word only when the shell would otherwise split or mangle
// it; the common names and paths pass through readable.
func shellWord(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`;&|<>()*?[]{}$!#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// roster is the ordered model list of a llama-swap config, from a real YAML
// parse. The line walker never parses keys itself.
func roster(text string) ([]string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		return nil, fmt.Errorf("not valid YAML: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, errors.New("empty document")
	}
	doc := root.Content[0]
	if doc.Kind != yaml.MappingNode {
		return nil, errors.New("document is not a mapping")
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value != "models" {
			continue
		}
		v := doc.Content[i+1]
		if v.Kind != yaml.MappingNode {
			return nil, errors.New("models: is not a mapping")
		}
		names := make([]string, 0, len(v.Content)/2)
		for j := 0; j+1 < len(v.Content); j += 2 {
			names = append(names, v.Content[j].Value)
		}
		return names, nil
	}
	return nil, nil
}

// verify re-parses the edited text and checks the roster order and every
// wrappable model's cmd against the parse, not against the line walker.
func verify(text string, wantOrder []string, want map[string]string) error {
	names, err := roster(text)
	if err != nil {
		return fmt.Errorf("edited file does not parse: %w", err)
	}
	if len(names) != len(wantOrder) {
		return fmt.Errorf("edited roster has %d models, want %d", len(names), len(wantOrder))
	}
	for i := range names {
		if names[i] != wantOrder[i] {
			return fmt.Errorf("edited roster reorders %q to %q", wantOrder[i], names[i])
		}
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		return err
	}
	doc := root.Content[0]
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value != "models" {
			continue
		}
		models := doc.Content[i+1]
		for j := 0; j+1 < len(models.Content); j += 2 {
			name := models.Content[j].Value
			frag, ok := want[name]
			if !ok {
				continue
			}
			if !strings.Contains(modelCmd(models.Content[j+1]), frag) {
				return fmt.Errorf("edited cmd of %q does not carry %q", name, frag)
			}
		}
	}
	return nil
}

// modelCmd is a model node's cmd value as llama-swap would read it.
func modelCmd(model *yaml.Node) string {
	if model.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(model.Content); i += 2 {
		if model.Content[i].Value == "cmd" {
			return model.Content[i+1].Value
		}
	}
	return ""
}

// modelKeyRows finds the line of every depth-1 key under models:, in order,
// and the line where the block ends. The count has to match the parsed roster,
// so a line the walker cannot follow is an error, never a silent miswire.
func modelKeyRows(lines []string, want int) (rows []int, blockEnd int, err error) {
	modelsRow := -1
	for i, line := range lines {
		if leadingSpaces(line) == 0 && strings.HasPrefix(line, "models:") {
			modelsRow = i
			break
		}
	}
	if modelsRow < 0 {
		return nil, 0, errors.New("no models: line at the top level")
	}
	indent := -1
	blockEnd = len(lines)
	for i := modelsRow + 1; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		ind := leadingSpaces(line)
		if ind == 0 {
			blockEnd = i
			break
		}
		if indent < 0 {
			indent = ind
		}
		if ind != indent {
			continue // a nested key, or a block scalar's content
		}
		after, ok := afterKey(trimmed)
		if !ok {
			continue // not a key line; the count check catches a real miss
		}
		if after != "" && !strings.HasPrefix(after, "#") {
			return nil, 0, fmt.Errorf("line %d: model %q is not a block mapping; wire only edits block style", i+1, trimmed)
		}
		rows = append(rows, i)
	}
	if len(rows) != want {
		return nil, 0, fmt.Errorf("found %d model key lines, the parsed roster has %d; open an issue with the config shape", len(rows), want)
	}
	return rows, blockEnd, nil
}

// afterKey splits a model key line at its colon: `Name:` and `"Quoted Name":`
// give "" (an optional comment may follow), anything else gives its tail.
func afterKey(trimmed string) (string, bool) {
	if strings.HasPrefix(trimmed, `"`) || strings.HasPrefix(trimmed, "'") {
		q := trimmed[0]
		end := strings.IndexByte(trimmed[1:], q)
		if end < 0 {
			return "", false
		}
		rest := trimmed[end+2:]
		if !strings.HasPrefix(rest, ":") {
			return "", false
		}
		return strings.TrimSpace(rest[1:]), true
	}
	idx := strings.Index(trimmed, ":")
	if idx <= 0 {
		return "", false
	}
	return strings.TrimSpace(trimmed[idx+1:]), true
}

func lineEnding(line string) string {
	if strings.HasSuffix(line, "\r") {
		return "\r"
	}
	return ""
}

func leadingSpaces(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// splitLines splits on \n, keeping a possible \r with its line, and reports
// whether the file ended with a newline so the join can put it back.
func splitLines(text string) ([]string, bool) {
	if text == "" {
		return nil, false
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		return lines[:len(lines)-1], true
	}
	return lines, false
}

func joinLines(lines []string, trailingNL bool) string {
	s := strings.Join(lines, "\n")
	if trailingNL {
		s += "\n"
	}
	return s
}
