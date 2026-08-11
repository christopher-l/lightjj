package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// rg-backed go-to-definition (Tier 1 — heuristic, no language server). Runs a
// per-language definition regex against the WORKING COPY via rg. Doesn't resolve
// imports/shadowing/overloads; for code review ("what does this call do?") the
// signature line + leading doc comment covers the common case. Resolves against
// @ regardless of which revision the diff view is showing — acceptable for v1;
// archaeology on old commits will see today's definition.
//
// rg goes through RunRaw so it executes in RepoDir (local) or on the remote
// host (SSH) — same sidecar pattern as gh.

// commentSyntax describes what counts as a doc-comment line preceding a
// definition. Prefix/suffix checks on the trimmed line — no parsing.
type commentSyntax struct {
	line       []string // line-comment prefixes ("//" also covers "///" and "//!")
	blockStart string   // "/*" — empty when the language has no block comments
	blockEnd   string   // "*/"
	decorators []string // prefixes of lines that sit between doc and def (@decorator, #[attr]) — kept, don't break the run
}

var (
	slashComments = &commentSyntax{line: []string{"//"}, blockStart: "/*", blockEnd: "*/", decorators: []string{"@"}}
	hashComments  = &commentSyntax{line: []string{"#"}, decorators: []string{"@"}}
	rustComments  = &commentSyntax{line: []string{"//"}, blockStart: "/*", blockEnd: "*/", decorators: []string{"#[", "#!["}}
)

type symbolLang struct {
	rgTypes []string // rg --type names, unioned (verified against `rg --type-list`)
	typeAdd string   // optional --type-add glob for types older rg builds don't ship
	// def: definition regex with %s (or %[1]s when the name appears more than
	// once) for the regex-escaped identifier. Anchored at line start
	// (optionally after leading whitespace) so call sites don't match.
	def     string
	comment *commentSyntax // nil → Context passes through untrimmed
	// normalize: optional pre-validation rewrite of the hovered token text
	// (the frontend sends the highlighted span's textContent verbatim). Output
	// still goes through identRe, so this can only narrow, never widen.
	normalize func(string) string
}

// tsDef is shared by typescript and svelte (<script lang="ts"> bodies). Svelte
// prepends `\s*` because script content is conventionally indented.
const tsDef = `(export\s+)?(declare\s+)?(async\s+)?(function|class|interface|type|enum|const|let|var)\s+%s\b`

var shellIdentPrefix = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)

// bashSymbolName: the shell tokenizer's hoverable span is the variable USE
// including its sigil (`$FOO`, `${FOO}`, `${FOO:-x}`), which never appears
// at the `FOO=` definition. Unwrap to the bare name; anything that isn't an
// identifier after unwrapping (`$1`, `$@`) comes back empty → identRe 400.
func bashSymbolName(s string) string {
	if strings.HasPrefix(s, "${") {
		s = strings.TrimSuffix(s[2:], "}")
	} else {
		s = strings.TrimPrefix(s, "$")
	}
	return shellIdentPrefix.FindString(s)
}

// symbolLangs: keys match LANGUAGES in frontend/src/lib/languages.ts — the
// frontend sends detectLanguage(filePath) as `lang`, so a key with no
// LANGUAGES entry is unreachable. Reachability also needs the highlighter to
// stamp data-sym on that language's identifiers (tok-variableName/typeName/
// propertyName/className spans) — a tokenizer that leaves identifiers
// unclassed makes its entry here dead.
var symbolLangs = map[string]symbolLang{
	"go":         {rgTypes: []string{"go"}, def: `^(func(\s+\([^)]+\))?|type|var|const)\s+%s\b`, comment: slashComments},
	"typescript": {rgTypes: []string{"ts"}, def: `^` + tsDef, comment: slashComments},
	"javascript": {rgTypes: []string{"js"}, def: `^(export\s+)?(async\s+)?(function|class|const|let|var)\s+%s\b`, comment: slashComments},
	// Python docstrings FOLLOW the def line; rg -B only yields before-context,
	// so only leading # comments (and @decorators) survive the trim.
	"python": {rgTypes: []string{"py"}, def: `^\s*(async\s+)?(def|class)\s+%s\b`, comment: hashComments},
	"rust":   {rgTypes: []string{"rust"}, def: `^\s*(pub(\([^)]+\))?\s+)?(async\s+)?(fn|struct|enum|trait|type|const|static|mod)\s+%s\b`, comment: rustComments},
	// swift: `let|var` is deliberately kept although it matches every local
	// binding of the same name (noisy for common names) — dropping it would
	// lose stored properties and top-level constants, the more useful hits.
	// No cheap discriminator: property vs local differ only by enclosing scope.
	"swift": {rgTypes: []string{"swift"},
		def:     `^\s*((public|private|fileprivate|internal|open|static|final|override|mutating|indirect|class)\s+)*(func|struct|class|enum|protocol|actor|extension|typealias|let|var|case)\s+%s\b`,
		comment: slashComments},
	"zig":      {rgTypes: []string{"zig"}, def: `^\s*(pub\s+)?((export|extern|inline)\s+)?(fn|const|var)\s+%s\b`, comment: slashComments},
	"protobuf": {rgTypes: []string{"protobuf"}, def: `^\s*(message|enum|service|rpc|oneof|extend)\s+%s\b`, comment: slashComments},
	// bash: `function NAME`, `NAME()`, and `[export|readonly|local|declare [-flags]] NAME=`.
	"bash": {rgTypes: []string{"sh"},
		def:       `^\s*(function\s+%[1]s\b|%[1]s\s*\(\s*\)|((export|readonly|local|declare)(\s+-\w+)*\s+)?%[1]s=)`,
		comment:   hashComments,
		normalize: bashSymbolName},
	// svelte: definitions a component references mostly live in .ts modules,
	// so search both types (rg unions repeated --type). rg ≥13 ships a svelte
	// type; --type-add appends the glob either way, so older builds work too.
	// In-component definitions sit in the <script> block → TS shapes, indented.
	"svelte": {rgTypes: []string{"svelte", "ts"}, typeAdd: "svelte:*.svelte", def: `^\s*` + tsDef, comment: slashComments},
}

// identRe gates the `name` query param. Symbol must be a plausible identifier
// — keeps regex injection out of the rg pattern (we also QuoteMeta, but
// belt-and-suspenders) and rejects nonsense like hovering whitespace.
var identRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

type SymbolHit struct {
	File    string   `json:"file"`
	Line    int      `json:"line"`
	Text    string   `json:"text"`    // the matching line (signature)
	Context []string `json:"context"` // leading lines (doc comment), source order
}

const (
	symbolMaxHits    = 20
	symbolContextPre = 6
)

func symbolRgArgs(spec symbolLang, name string) []string {
	argv := []string{
		"rg", "--json", "-m", fmt.Sprint(symbolMaxHits),
		"-B", fmt.Sprint(symbolContextPre),
	}
	if spec.typeAdd != "" {
		argv = append(argv, "--type-add", spec.typeAdd)
	}
	for _, t := range spec.rgTypes {
		argv = append(argv, "--type", t)
	}
	return append(argv,
		"-e", fmt.Sprintf(spec.def, regexp.QuoteMeta(name)),
		// Explicit path: RunRaw → runSeparate always sets cmd.Stdin (even
		// empty), and rg with non-tty stdin + no path reads STDIN instead of
		// cwd. Without this rg searches the empty pipe and finds nothing.
		"./",
	)
}

func (s *Server) handleSymbol(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	spec, ok := symbolLangs[r.URL.Query().Get("lang")]
	if ok && spec.normalize != nil {
		name = spec.normalize(name)
	}
	if !identRe.MatchString(name) {
		s.writeError(w, http.StatusBadRequest, "name must be an identifier")
		return
	}
	if !ok {
		s.writeJSON(w, r, http.StatusOK, map[string]any{"hits": []SymbolHit{}})
		return
	}
	out, err := s.Runner.RunRaw(r.Context(), symbolRgArgs(spec, name))
	// rg exits 1 on no-match. LocalRunner.runSeparate discards stdout on
	// non-zero exit and folds it into the error string, so the no-match path
	// (exit 1, JSON summary on stdout, nothing on stderr) arrives here as
	// err="exit code 1: {summary json...}". Recover by parsing the error
	// text — it's still the same line-delimited JSON. rg-not-installed and
	// real errors yield non-JSON text → parseRgJSON returns []. Degrade
	// silently; hover-docs are best-effort like PR badges.
	if err != nil && len(out) == 0 {
		out = []byte(err.Error())
	}
	hits := parseRgJSON(out)
	for i := range hits {
		hits[i].Context = trimDocComment(hits[i].Context, spec.comment)
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"hits": hits})
}

// trimDocComment narrows raw rg -B context to the trailing contiguous run of
// comment lines (plus decorator/attribute lines) immediately preceding the
// definition. A blank line or any code line ends the run, so a stray `}` from
// the previous declaration never reaches the hover card; no leading comment →
// empty. nil syntax → ctx unchanged (language with no comment config).
//
// Block comments count only when they stand alone: a `*/`-terminated line
// whose `/*` opener sits mid-line (`x := 1 /* note */`), or a multi-line
// block whose opener turns out to trail code (`b := f() /* start`), is code
// — the run stops below it and the block's lines are not kept.
func trimDocComment(ctx []string, c *commentSyntax) []string {
	if c == nil {
		return ctx
	}
	hasPrefix := func(s string, ps []string) bool {
		for _, p := range ps {
			if strings.HasPrefix(s, p) {
				return true
			}
		}
		return false
	}
	keepFrom := len(ctx)
	inBlock := false       // walking upward through a /* … */ body toward its opener
	belowBlock := keepFrom // keepFrom before entering the block — restored if the block proves to be code
	for i := len(ctx) - 1; i >= 0; i-- {
		line := strings.TrimSpace(ctx[i])
		switch {
		case inBlock:
			switch {
			case strings.HasPrefix(line, c.blockStart):
				inBlock = false
			case strings.Contains(line, c.blockStart): // opener trails code
				return ctx[belowBlock:]
			}
		case line == "":
			return ctx[keepFrom:]
		case hasPrefix(line, c.line), hasPrefix(line, c.decorators):
		case c.blockEnd != "" && strings.HasSuffix(line, c.blockEnd):
			switch strings.Index(line, c.blockStart) {
			case 0: // `/** one-liner */`
			case -1: // closer of a multi-line block; find its opener above
				inBlock, belowBlock = true, keepFrom
			default: // `code /* trailing note */`
				return ctx[keepFrom:]
			}
		default:
			return ctx[keepFrom:]
		}
		keepFrom = i
	}
	// Still inBlock here = opener above the -B window; what we saw is all
	// comment body, keep it.
	return ctx[keepFrom:]
}

// parseRgJSON walks rg --json line-delimited output. We only care about
// context-before + match; rg interleaves them per file in order, so a small
// rolling buffer of context lines flushed at each match is enough.
func parseRgJSON(out []byte) []SymbolHit {
	type rgLine struct {
		Type string `json:"type"`
		Data struct {
			Path       struct{ Text string } `json:"path"`
			LineNumber int                   `json:"line_number"`
			Lines      struct{ Text string } `json:"lines"`
		} `json:"data"`
	}
	hits := []SymbolHit{}
	ctx := []string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var l rgLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case "context":
			ctx = append(ctx, strings.TrimRight(l.Data.Lines.Text, "\n"))
		case "match":
			hits = append(hits, SymbolHit{
				File:    strings.TrimPrefix(l.Data.Path.Text, "./"),
				Line:    l.Data.LineNumber,
				Text:    strings.TrimRight(l.Data.Lines.Text, "\n"),
				Context: ctx,
			})
			ctx = []string{}
		case "end":
			ctx = []string{}
		}
		if len(hits) >= symbolMaxHits {
			break
		}
	}
	return hits
}
