package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"

	"github.com/chronologos/lightjj/testutil"
	"github.com/stretchr/testify/assert"
)

func TestParseRgJSON(t *testing.T) {
	out := []byte(`{"type":"begin","data":{"path":{"text":"a.go"}}}
{"type":"context","data":{"path":{"text":"a.go"},"lines":{"text":"// Doc line\n"},"line_number":9}}
{"type":"context","data":{"path":{"text":"a.go"},"lines":{"text":"// returns X\n"},"line_number":10}}
{"type":"match","data":{"path":{"text":"a.go"},"lines":{"text":"func Foo() int {\n"},"line_number":11}}
{"type":"end","data":{"path":{"text":"a.go"}}}
{"type":"begin","data":{"path":{"text":"b.go"}}}
{"type":"match","data":{"path":{"text":"b.go"},"lines":{"text":"type Foo struct{}\n"},"line_number":3}}
{"type":"end","data":{"path":{"text":"b.go"}}}
`)
	hits := parseRgJSON(out)
	assert.Len(t, hits, 2)
	assert.Equal(t, SymbolHit{
		File: "a.go", Line: 11, Text: "func Foo() int {",
		Context: []string{"// Doc line", "// returns X"},
	}, hits[0])
	assert.Equal(t, SymbolHit{File: "b.go", Line: 3, Text: "type Foo struct{}", Context: []string{}}, hits[1])
}

func TestParseRgJSON_ContextResetsBetweenFiles(t *testing.T) {
	// Dangling context (no following match in that file) must NOT bleed into
	// the next file's hit.
	out := []byte(`{"type":"context","data":{"path":{"text":"a.go"},"lines":{"text":"stale\n"},"line_number":1}}
{"type":"end","data":{"path":{"text":"a.go"}}}
{"type":"match","data":{"path":{"text":"b.go"},"lines":{"text":"func Foo()\n"},"line_number":1}}
`)
	hits := parseRgJSON(out)
	assert.Len(t, hits, 1)
	assert.Empty(t, hits[0].Context)
}

func TestHandleSymbol_RejectsNonIdentifier(t *testing.T) {
	srv := newTestServer(testutil.NewMockRunner(t))
	for _, bad := range []string{"", "foo.bar", "a b", "x;rm -rf"} {
		w := httptest.NewRecorder()
		srv.Mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/symbol?name="+url.QueryEscape(bad)+"&lang=go", nil))
		assert.Equal(t, http.StatusBadRequest, w.Code, bad)
	}
}

func TestHandleSymbol_UnsupportedLangReturnsEmpty(t *testing.T) {
	srv := newTestServer(testutil.NewMockRunner(t))
	w := httptest.NewRecorder()
	srv.Mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/symbol?name=Foo&lang=cobol", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct{ Hits []SymbolHit }
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Empty(t, resp.Hits)
}

func TestHandleSymbol_BuildsRgArgv(t *testing.T) {
	r := testutil.NewMockRunner(t)
	defer r.Verify()
	// QuoteMeta is a no-op on plain identifiers, so the pattern is predictable.
	r.Expect([]string{
		"rg", "--json", "-m", "20", "-B", "6", "--type", "go",
		"-e", `^(func(\s+\([^)]+\))?|type|var|const)\s+Foo\b`,
		"./",
	}).SetOutput([]byte(`{"type":"match","data":{"path":{"text":"x.go"},"lines":{"text":"func Foo()\n"},"line_number":1}}`))
	srv := newTestServer(r)
	w := httptest.NewRecorder()
	srv.Mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/symbol?name=Foo&lang=go", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct{ Hits []SymbolHit }
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, []SymbolHit{{File: "x.go", Line: 1, Text: "func Foo()", Context: []string{}}}, resp.Hits)
}

func TestHandleSymbol_SvelteAddsTypeAndTrimsContext(t *testing.T) {
	r := testutil.NewMockRunner(t)
	defer r.Verify()
	r.Expect([]string{
		"rg", "--json", "-m", "20", "-B", "6",
		"--type-add", "svelte:*.svelte", "--type", "svelte", "--type", "ts",
		"-e", `^\s*` + fmt.Sprintf(tsDef, "openTab"),
		"./",
	}).SetOutput([]byte(`{"type":"context","data":{"path":{"text":"A.svelte"},"lines":{"text":"  }\n"},"line_number":7}}
{"type":"context","data":{"path":{"text":"A.svelte"},"lines":{"text":"  // Opens a tab.\n"},"line_number":8}}
{"type":"match","data":{"path":{"text":"A.svelte"},"lines":{"text":"  function openTab(p: string) {\n"},"line_number":9}}`))
	srv := newTestServer(r)
	w := httptest.NewRecorder()
	srv.Mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/symbol?name=openTab&lang=svelte", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	var resp struct{ Hits []SymbolHit }
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, []SymbolHit{{
		File: "A.svelte", Line: 9, Text: "  function openTab(p: string) {",
		Context: []string{"  // Opens a tab."},
	}}, resp.Hits)
}

func TestBashSymbolName(t *testing.T) {
	for in, want := range map[string]string{
		"FOO": "FOO", "$FOO": "FOO", "${FOO}": "FOO", "${FOO:-default}": "FOO", "${#arr}": "",
		"$1": "", "$@": "", "$": "", "build": "build", "_x9": "_x9",
	} {
		assert.Equal(t, want, bashSymbolName(in), in)
	}
}

func TestHandleSymbol_BashUnwrapsSigilBeforeValidation(t *testing.T) {
	bashDef := symbolLangs["bash"].def
	for _, tc := range []struct{ sent, searched string }{
		{"$OUT_DIR", "OUT_DIR"}, {"${OUT_DIR}", "OUT_DIR"}, {"detect_platform", "detect_platform"},
	} {
		r := testutil.NewMockRunner(t)
		r.Expect([]string{
			"rg", "--json", "-m", "20", "-B", "6", "--type", "sh",
			"-e", fmt.Sprintf(bashDef, tc.searched), "./",
		}).SetOutput([]byte(``))
		srv := newTestServer(r)
		w := httptest.NewRecorder()
		srv.Mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/symbol?lang=bash&name="+url.QueryEscape(tc.sent), nil))
		assert.Equal(t, http.StatusOK, w.Code, tc.sent)
		r.Verify()
	}
	// Positional/special params unwrap to nothing → rejected, rg never runs.
	srv := newTestServer(testutil.NewMockRunner(t))
	for _, bad := range []string{"$1", "$@", "${#x}"} {
		w := httptest.NewRecorder()
		srv.Mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/symbol?lang=bash&name="+url.QueryEscape(bad), nil))
		assert.Equal(t, http.StatusBadRequest, w.Code, bad)
	}
	// normalize is bash-only: `$` stays a legal JS identifier char elsewhere.
	assert.Nil(t, symbolLangs["typescript"].normalize)
}

func TestSymbolLangs_CompileForAllLangs(t *testing.T) {
	// rg uses Rust regex; Go's engine is close enough to catch unbalanced
	// groups/brackets at template-authoring time.
	for lang, spec := range symbolLangs {
		assert.NotEmpty(t, spec.rgTypes, "missing rg --type for %s", lang)
		pat := fmt.Sprintf(spec.def, "sampleName")
		assert.NotContains(t, pat, "%!", "bad fmt verb in %s template", lang)
		_, err := regexp.Compile(pat)
		assert.NoError(t, err, lang)
	}
}

func TestSymbolLangs_DefPatterns(t *testing.T) {
	cases := []struct {
		lang, name string
		match      []string
		noMatch    []string
	}{
		{"swift", "Foo",
			[]string{"struct Foo {", "public final class Foo: Bar {", "  static func Foo() -> Int {", "enum Foo {", "protocol Foo {", "  case Foo"},
			[]string{"  let x = Foo()", "  return Foo(a: 1)", "Foo.shared.run()"}},
		{"zig", "init",
			[]string{"pub fn init(alloc: Allocator) Self {", "    fn init() void {", "pub const init = 3;", "export fn init() void {"},
			[]string{"    const x = init();", "    self.init();", "    return init(a);"}},
		{"zig", "Point", []string{"const Point = struct {", "pub const Point = struct {"}, []string{"    var p: Point = .{};"}},
		{"protobuf", "User",
			[]string{"message User {", "  enum User {", "service User {", "  rpc User(Req) returns (Resp);"},
			[]string{"  User user = 1;", "  repeated User users = 2;"}},
		{"bash", "build",
			[]string{"build() {", "function build {", "function build() {", "  build () {", "build=1", "export build=/tmp", "declare -r build=x", "local build=y",
				"  local -r build=z", "readonly -a build=(1 2)", "export -n build=", "declare -g -A build=()"},
			[]string{"  build --fast", "echo build=1", "x=$(build)", "rebuild() {", "local rebuild=1", "unset build"}},
		{"svelte", "openTab",
			[]string{"  function openTab(path: string) {", "  const openTab = (p: string) => {", "  export function openTab() {", "  let openTab = $state(false)"},
			[]string{"    openTab(ws.path)", "  onclick={() => openTab(p)}", "    await openTab(x)"}},
	}
	for _, tc := range cases {
		spec, ok := symbolLangs[tc.lang]
		if !assert.True(t, ok, tc.lang) {
			continue
		}
		re := regexp.MustCompile(fmt.Sprintf(spec.def, regexp.QuoteMeta(tc.name)))
		for _, s := range tc.match {
			assert.True(t, re.MatchString(s), "%s: should match %q", tc.lang, s)
		}
		for _, s := range tc.noMatch {
			assert.False(t, re.MatchString(s), "%s: should NOT match %q", tc.lang, s)
		}
	}
}

func TestTrimDocComment(t *testing.T) {
	cases := []struct {
		name string
		c    *commentSyntax
		in   []string
		want []string
	}{
		{"go doc comment kept, preceding decl dropped",
			slashComments,
			[]string{"\treturn x", "}", "", "// Foo does X.", "// It returns Y."},
			[]string{"// Foo does X.", "// It returns Y."}},
		{"ts: brace + blank + JSDoc block → only the block",
			slashComments,
			[]string{"  x()", "}", "", "/**", " * Does a thing.", " * @param a thing", " */"},
			[]string{"/**", " * Does a thing.", " * @param a thing", " */"}},
		{"ts: one-line JSDoc",
			slashComments,
			[]string{"}", "/** One-liner. */"},
			[]string{"/** One-liner. */"}},
		{"ts: block opener beyond the window keeps everything seen",
			slashComments,
			[]string{" * mid", " * more", " */"},
			[]string{" * mid", " * more", " */"}},
		{"trailing block comment on a code line is code, not doc",
			slashComments,
			[]string{"// real doc?", "x := 1 /* note */"},
			[]string{}},
		{"one-line block directly above is kept even after code-with-trailing-comment",
			slashComments,
			[]string{"y := 2 /* n */", "/* Doc. */"},
			[]string{"/* Doc. */"}},
		{"multi-line block whose opener trails code: block dropped, run stops",
			slashComments,
			[]string{"// unrelated", "b := foo() /* start", "   still note", "*/", "// doc"},
			[]string{"// doc"}},
		{"multi-line block whose opener trails code, directly above def → empty",
			slashComments,
			[]string{"a := 1", "b := foo() /* start", "end */"},
			[]string{}},
		{"ts: decorator between JSDoc and def is kept",
			slashComments,
			[]string{"}", "/** Component. */", "@Injectable()"},
			[]string{"/** Component. */", "@Injectable()"}},
		{"py: decorator preserved, comment above it kept",
			hashComments,
			[]string{"    return 1", "", "# Cached lookup.", "@functools.cache"},
			[]string{"# Cached lookup.", "@functools.cache"}},
		{"rust: /// docs + #[attr] kept",
			rustComments,
			[]string{"}", "", "/// A point.", "#[derive(Debug)]"},
			[]string{"/// A point.", "#[derive(Debug)]"}},
		{"blank line breaks adjacency",
			slashComments,
			[]string{"// unrelated", "", "// doc"},
			[]string{"// doc"}},
		{"no comment precedes → empty",
			slashComments,
			[]string{"\tx := 1", "}", ""},
			[]string{}},
		{"code directly above → empty",
			slashComments,
			[]string{"const a = 1"},
			[]string{}},
		{"empty in → empty out", slashComments, []string{}, []string{}},
		{"nil syntax → unchanged",
			nil,
			[]string{"}", "", "whatever"},
			[]string{"}", "", "whatever"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trimDocComment(tc.in, tc.c)
			assert.Equal(t, tc.want, got)
			assert.NotNil(t, got, "must serialize as [] not null")
		})
	}
}
