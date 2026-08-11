package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// CLI subcommands for agent harnesses with curl/wget denylisted in their Bash
// sandbox. `lightjj api METHOD PATH [BODY]` makes an HTTP request via Go's
// net/http to a discovered local lightjj instance; `lightjj sessions` lists
// running instances. See docs/design-notes/api-cli.md for the security model
// and discovery algorithm.

// headerSlice is a repeatable -H "Key: Value" flag value.
type headerSlice []string

func (h *headerSlice) String() string { return strings.Join(*h, ", ") }
func (h *headerSlice) Set(v string) error {
	*h = append(*h, v)
	return nil
}

// validateAddr parses and validates a host:port address. The host must be
// loopback (127.0.0.1, ::1, or localhost) and the port must be 1..65535.
// Both checks are load-bearing: net.SplitHostPort("127.0.0.1:80@evil.com")
// returns (host="127.0.0.1", port="80@evil.com", err=nil) — the strconv check
// rejects it, not the host check. See Security model §2 in api-cli.md.
func validateAddr(addr string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(addr)
	if err != nil {
		return "", "", fmt.Errorf("invalid address %q: %v", addr, err)
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		// ok
	default:
		return "", "", fmt.Errorf("address %q is not loopback (must be 127.0.0.1, ::1, or localhost)", addr)
	}
	n, perr := strconv.Atoi(port)
	if perr != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("invalid port %q in address %q", port, addr)
	}
	// Return the canonical port, not the raw string — Atoi accepts "080" and
	// "+80", which downstream parsers (url.Parse) may handle differently.
	// Validate-then-use-original is the textbook parser-mismatch bug shape.
	return host, strconv.Itoa(n), nil
}

// containsPath reports whether resolvedCwd is inside (or equal to)
// resolvedRepoDir. Both paths must already be EvalSymlinks-resolved. The check
// is component-aware (filepath.Rel), not byte-prefix — `/a/..foo` is *inside*
// `/a`; `/a/foobar` is not inside `/a/foo`.
func containsPath(resolvedRepoDir, resolvedCwd string) bool {
	rel, err := filepath.Rel(resolvedRepoDir, resolvedCwd)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// candidate pairs a session with the tab whose path matched and that path's
// resolved form (for sorting and tie detection).
type candidate struct {
	sess     sessionInfo
	tab      sessionTab
	resolved string
}

// resolveCandidatePath applies the per-path discovery filters to one session
// path (RepoDir or a tab path): pre-filter "/"/relative/empty BEFORE any
// resolution ("/" would universally match any cwd; a single-component path
// like "/Users" is one level less universal and is NOT rejected — it's
// constrained by the dir trust boundary and "longest path wins" prefers any
// real session over it), EvalSymlinks (skip on failure — deleted dir), then
// re-check the RESOLVED path != "/" (a symlink-to-root slips past the
// pre-filter, which sees the unresolved string). ok=false → skip this path.
func resolveCandidatePath(p string) (resolved string, ok bool) {
	cleaned := filepath.Clean(p)
	if cleaned == "" || cleaned == "/" || !filepath.IsAbs(cleaned) {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil || filepath.Clean(resolved) == "/" {
		return "", false
	}
	return resolved, true
}

// candidateTabs lists the paths a session can be matched by. Files from
// binaries that write `tabs` are matched by tab (tab 0 == RepoDir is in the
// list); older files carry only RepoDir, which IS tab 0 on every version that
// writes session files — so it's synthesized as {ID:"0"} and the caller
// needn't special-case the old format. Tab ids are embedded into the request
// path later, so a non-numeric id (corruption/plant) drops the tab here.
func candidateTabs(s sessionInfo) []sessionTab {
	if len(s.Tabs) == 0 {
		return []sessionTab{{ID: "0", Path: s.RepoDir}}
	}
	out := make([]sessionTab, 0, len(s.Tabs))
	for _, t := range s.Tabs {
		if n, err := strconv.Atoi(t.ID); err != nil || n < 0 || strconv.Itoa(n) != t.ID {
			continue
		}
		out = append(out, t)
	}
	return out
}

// discoverSession finds the most-specific running local lightjj instance with
// an open tab whose path contains repoPath, and returns that session plus the
// matched tab. Implements the 7-step discovery algorithm in
// docs/design-notes/api-cli.md, applied per tab path (RepoDir for pre-tabs
// session files). Within one session the deepest matching tab wins (nested
// repos open as two tabs); across sessions the deepest match wins and a tie
// is an error. dir is taken as a parameter (not resolved internally) so tests
// can pass t.TempDir().
func discoverSession(dir, repoPath string) (sessionInfo, sessionTab, error) {
	// Step 1: read + size-cap + schema filter.
	sessions, err := readSessions(dir)
	if err != nil {
		return sessionInfo{}, sessionTab{}, fmt.Errorf("reading sessions: %w", err)
	}

	// Resolve repoPath once. Abs FIRST, then EvalSymlinks — EvalSymlinks of a
	// relative path returns a relative result without erroring, so an "Abs as
	// fallback" never fires and a relative `--repo .` would silently match
	// nothing (filepath.Rel against an absolute RepoDir errors → no match).
	// EvalSymlinks both sides so the matcher is independent of whether
	// `jj workspace root` (the RepoDir producer) resolves symlinks.
	if abs, aerr := filepath.Abs(repoPath); aerr == nil {
		repoPath = abs
	}
	resolvedCwd, rerr := filepath.EvalSymlinks(repoPath)
	if rerr != nil {
		// cwd deleted or perms — fall back to the unresolved absolute path.
		resolvedCwd = repoPath
		if !filepath.IsAbs(resolvedCwd) {
			return sessionInfo{}, sessionTab{}, errors.New("cannot determine working directory")
		}
	}

	var alive []sessionInfo // for the zero-match error message (incl. SSH)
	var matches []candidate // at most one per session: its deepest matching tab

	for _, s := range sessions {
		// Step 1 (continued): a session whose own RepoDir is "/"/relative/
		// empty is corrupt or planted — drop it wholesale, tabs and all.
		if cleaned := filepath.Clean(s.RepoDir); cleaned == "" || cleaned == "/" || !filepath.IsAbs(cleaned) {
			continue
		}
		// Step 2: filter dead pids — freshness, not trust (Security §3).
		if !pidAlive(s.PID) {
			continue
		}
		alive = append(alive, s)
		// Step 3: filter Mode == "local". An SSH-mode session holds *remote*
		// paths (RepoDir and every tab); if one coincidentally exists locally,
		// a containment match would route the agent's writes to the wrong
		// machine's repo. Dropped before any path is looked at.
		if s.Mode != "local" {
			continue
		}
		// Step 4: validate Addr (Security §2). Reject and skip on failure.
		if _, _, err := validateAddr(s.Addr); err != nil {
			continue
		}
		// Step 5: containment match, per tab path. Keep this session's
		// deepest matching tab — nested repos (cwd inside both /a and /a/b,
		// each open as a tab) must target the inner one.
		var best *candidate
		for _, tab := range candidateTabs(s) {
			resolved, ok := resolveCandidatePath(tab.Path)
			if !ok || !containsPath(resolved, resolvedCwd) {
				continue
			}
			if best == nil || len(resolved) > len(best.resolved) {
				best = &candidate{sess: s, tab: tab, resolved: resolved}
			}
		}
		if best != nil {
			matches = append(matches, *best)
		}
	}

	// Step 7: zero matches.
	if len(matches) == 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "no running lightjj session matches %s", repoPath)
		if len(alive) > 0 {
			b.WriteString("\nrunning sessions:")
			for _, s := range alive {
				fmt.Fprintf(&b, "\n  pid %d  %s  %s  %s", s.PID, s.Addr, s.Mode, s.RepoDir)
				for _, tab := range s.Tabs {
					if tab.ID != "0" {
						fmt.Fprintf(&b, "\n    tab %s  %s", tab.ID, tab.Path)
					}
				}
			}
		}
		b.WriteString("\nuse --repo to match a different path, --addr to bypass discovery, or start lightjj in the repo")
		return sessionInfo{}, sessionTab{}, errors.New(b.String())
	}

	// Step 6: most-specific (deepest) path wins. Among matches all paths are
	// ancestors of repoPath and therefore prefixes of one another, so byte
	// length agrees with component depth on the winner.
	sort.SliceStable(matches, func(i, j int) bool {
		return len(matches[i].resolved) > len(matches[j].resolved)
	})
	best := matches[0]
	// Sorted longest-first: a tie can only be matches[1] sharing matches[0]'s
	// resolved path (two lightjj instances with the same repo open). Don't
	// auto-pick — a stale instance could shadow a fresh one.
	if len(matches) > 1 && matches[1].resolved == best.resolved {
		var b strings.Builder
		fmt.Fprintf(&b, "multiple lightjj sessions match %s; use --addr to pick one:", best.resolved)
		for _, c := range matches {
			if c.resolved != best.resolved {
				break
			}
			fmt.Fprintf(&b, "\n  pid %d  %s  tab %s  started %s", c.sess.PID, c.sess.Addr, c.tab.ID, time.UnixMilli(c.sess.StartedAt).Format(time.RFC3339))
		}
		return sessionInfo{}, sessionTab{}, errors.New(b.String())
	}
	return best.sess, best.tab, nil
}

// resolveTabPath applies the discovered tab to a caller-supplied request
// path. A path of exactly "/api" or starting with "/api/" is tab-relative and
// gets the matched tab's "/tab/{id}" prefix (every root-mounted /api/* route
// — config, state — is also mounted per tab, so this never breaks a working
// path; it also retires the unprefixed-/api-returns-SPA-HTML footgun). An
// explicit "/tab/N/..." or any other path is left verbatim — the caller chose.
func resolveTabPath(path string, tab sessionTab) string {
	if tab.ID == "" {
		return path
	}
	if path == "/api" || strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/api?") {
		return "/tab/" + tab.ID + path
	}
	return path
}

// explicitTabID extracts N from a "/tab/N/..." path ("" if not that shape).
func explicitTabID(path string) string {
	rest, ok := strings.CutPrefix(path, "/tab/")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	id, _, _ = strings.Cut(id, "?")
	return id
}

// versionMismatchWarning returns the one-line stderr warning for a session
// written by a different lightjj version than this binary (the stale
// go:embed-binary bug class), or "" when they match or the session predates
// the version stamp.
func versionMismatchWarning(sess sessionInfo, self string) string {
	a, b := releaseCore(sess.Version), releaseCore(self)
	if a == "" || b == "" || a == b {
		return ""
	}
	return fmt.Sprintf("lightjj api: warning: session pid %d runs lightjj %s but this binary is %s — one of them is stale", sess.PID, sess.Version, self)
}

// releaseCore reduces a version string to its comparable X.Y.Z release core,
// or "" for anything that isn't a release build (dev / go-run "(devel)" /
// VCS pseudo-versions like 1.36.2-0.2026...-abc123). Only release-number
// differences indicate the stale-embedded-binary class; `+dirty` metadata,
// pre-release suffixes, and dev builds (the maintainer's two-terminal
// go-run-server + built-CLI loop) must not warn on every call.
func releaseCore(v string) string {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		if strings.HasPrefix(v[i:], "-0.") { // Go pseudo-version → not a release
			return ""
		}
		v = v[:i]
	}
	for _, part := range strings.Split(v, ".") {
		if part == "" {
			return ""
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return ""
			}
		}
	}
	if strings.Count(v, ".") != 2 {
		return ""
	}
	return v
}

// doAPIRequest builds and sends an HTTP request to a validated loopback
// address. The URL is constructed via url.URL with net.JoinHostPort — never
// string concatenation — so a malformed addr can't smuggle in a different host
// via userinfo syntax. path may include `?query`; url.Parse extracts both
// parts. Sets Content-Type: application/json when a body is present unless
// overridden via -H.
func doAPIRequest(addr, method, path string, body io.Reader, extraHeaders []string) (*http.Response, error) {
	host, port, err := validateAddr(addr)
	if err != nil {
		return nil, err
	}
	pu, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path %q: %v", path, err)
	}
	u := url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(host, port),
		Path:     pu.Path,
		RawQuery: pu.RawQuery,
	}
	req, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return nil, err
	}
	hasContentType := false
	for _, h := range extraHeaders {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return nil, fmt.Errorf("invalid header %q (want \"Key: Value\")", h)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if strings.EqualFold(k, "Content-Type") {
			hasContentType = true
		}
		// Add, not Set — repeated -H of the same key appends, matching curl.
		// Go's transport rejects \r/\n in header values at RoundTrip time, so
		// no manual injection check needed.
		req.Header.Add(k, v)
	}
	if body != nil && !hasContentType {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	return client.Do(req)
}

const apiUsage = `usage: lightjj api [flags] METHOD PATH [BODY]

  METHOD   GET | POST | PUT | DELETE | PATCH (case-insensitive, uppercased)
  PATH     URL path, including query string. Tab-scoped routes are
           /tab/{N}/api/...; a tab-relative /api/... is prefixed with the
           tab discovery matched for your cwd (any open tab, not just the
           launch repo). An explicit /tab/N/... is sent verbatim.
  BODY     literal JSON | @file (path relative to CWD) | "-" for stdin.

flags:
  --addr   host:port — bypass discovery entirely (PATH sent verbatim —
           spell out /tab/N/). Loopback only.
  --repo   path — match a different repo than cwd
  -H       "Key: Value" — extra header (repeatable)

flags must come before METHOD.
`

// runAPISubcommand implements `lightjj api`. Returns an exit code per the
// Output contract in api-cli.md: 0=2xx, 1=discovery/connection, 2=usage,
// 4=4xx, 5=5xx, else <400→0/≥400→1.
func runAPISubcommand(args []string) int {
	fs := flag.NewFlagSet("api", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	addrFlag := fs.String("addr", "", "host:port — bypass discovery (loopback only)")
	repoFlag := fs.String("repo", "", "match a different repo than cwd")
	var headers headerSlice
	fs.Var(&headers, "H", "extra header (repeatable)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, apiUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}

	pos := fs.Args()
	if len(pos) < 2 {
		fmt.Fprint(os.Stderr, apiUsage)
		return 2
	}
	if len(pos) > 3 {
		fmt.Fprintf(os.Stderr, "lightjj api: too many arguments (flags must come before METHOD)\n")
		return 2
	}
	method := strings.ToUpper(pos[0])
	path := pos[1]
	// Exactly one leading slash: "api/log" would skip the tab rewrite yet still
	// be sent as /api/log (SPA HTML, no hint); "//api/log" parses as an
	// authority and mangles the path.
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		fmt.Fprintf(os.Stderr, "lightjj api: PATH must start with a single '/' (got %q) — e.g. /api/log\n", path)
		return 2
	}

	// Resolve the target address: explicit --addr bypasses discovery entirely.
	var addr string
	if *addrFlag != "" {
		if _, _, err := validateAddr(*addrFlag); err != nil {
			fmt.Fprintf(os.Stderr, "lightjj api: %v\n", err)
			return 2
		}
		if *repoFlag != "" {
			fmt.Fprintln(os.Stderr, "lightjj api: note: --addr bypasses discovery, so --repo (and /api tab auto-prefixing) is ignored")
		}
		addr = *addrFlag
	} else {
		dir, err := sessionDirReadOnly()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(os.Stderr, "lightjj api: no running lightjj session found (session dir missing)")
			} else {
				fmt.Fprintf(os.Stderr, "lightjj api: %v\n", err)
			}
			return 1
		}
		repoPath := *repoFlag
		if repoPath == "" {
			repoPath, err = os.Getwd()
			if err != nil {
				fmt.Fprintf(os.Stderr, "lightjj api: cannot determine working directory: %v\n", err)
				return 1
			}
		}
		sess, tab, err := discoverSession(dir, repoPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lightjj api: %v\n", err)
			return 1
		}
		addr = sess.Addr
		if w := versionMismatchWarning(sess, resolvedVersion()); w != "" {
			fmt.Fprintln(os.Stderr, w)
		}
		// Target the matched tab: tab-relative /api/... paths get its prefix;
		// an explicit /tab/N/ is honoured. Silent for the launch tab (the
		// common case); a non-launch match is announced so an agent copying
		// /tab/0/ examples from the docs notices it is reading the wrong repo.
		resolved := resolveTabPath(path, tab)
		if tab.ID != "" && tab.ID != "0" {
			if explicit := explicitTabID(path); explicit != "" && explicit != tab.ID {
				fmt.Fprintf(os.Stderr, "lightjj api: cwd is in tab %s (%s) but PATH targets /tab/%s/ explicitly — pass /api/... to target tab %s\n", tab.ID, tab.Path, explicit, tab.ID)
			} else {
				fmt.Fprintf(os.Stderr, "lightjj api: tab %s (%s) → %s\n", tab.ID, tab.Path, resolved)
			}
		}
		path = resolved
	}

	// Resolve the body source.
	var bodyReader io.Reader
	if len(pos) == 3 {
		raw := pos[2]
		switch {
		case raw == "-":
			bodyReader = os.Stdin
		case strings.HasPrefix(raw, "@"):
			f, err := os.Open(raw[1:])
			if err != nil {
				fmt.Fprintf(os.Stderr, "lightjj api: %v\n", err)
				return 2
			}
			defer f.Close()
			bodyReader = f
		default:
			bodyReader = strings.NewReader(raw)
		}
	}

	resp, err := doAPIRequest(addr, method, path, bodyReader, headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lightjj api: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	// stdout: response body, always — even on 4xx/5xx (the API returns JSON
	// error objects that agents pipe to jq).
	_, _ = io.Copy(os.Stdout, resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return 0
	}
	fmt.Fprintf(os.Stderr, "HTTP %d %s\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	switch {
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return 4
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		return 5
	case resp.StatusCode < 400:
		return 0
	default:
		return 1
	}
}

// runSessionsSubcommand implements `lightjj sessions`. Lists ALL alive
// sessions including SSH-mode (which `lightjj api` discovery filters out).
// Sweeps stale entries first; the verifyOwnedDir hard-error in
// sessionDirReadOnly is what makes that os.Remove safe.
func runSessionsSubcommand(args []string) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dir, err := sessionDirReadOnly()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No session dir — no sessions. Exit 0 with empty output.
			if *jsonOut {
				fmt.Println("[]")
			}
			return 0
		}
		fmt.Fprintf(os.Stderr, "lightjj sessions: %v\n", err)
		return 1
	}
	sweepStaleSessions(dir)
	sessions, err := readSessions(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lightjj sessions: %v\n", err)
		return 1
	}
	live := []sessionInfo{}
	for _, s := range sessions {
		if pidAlive(s.PID) {
			live = append(live, s)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].PID < live[j].PID })

	if *jsonOut {
		b, _ := json.MarshalIndent(live, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PID\tADDR\tMODE\tVERSION\tREPO")
	for _, s := range live {
		v := s.Version
		if v == "" {
			v = "-" // pre-version-stamp binary
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", s.PID, s.Addr, s.Mode, v, s.RepoDir)
		// Extra tabs as continuation rows (tab 0 == REPO, already shown).
		for _, tab := range s.Tabs {
			if tab.ID != "0" {
				fmt.Fprintf(w, "\t\t\t\t  tab %s: %s\n", tab.ID, tab.Path)
			}
		}
	}
	w.Flush()
	return 0
}
