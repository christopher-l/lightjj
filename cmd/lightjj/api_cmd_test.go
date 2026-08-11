package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubPidAlive replaces pidAlive for the duration of the test. alive holds
// the pids considered alive; everything else is dead.
func stubPidAlive(t *testing.T, alive ...int) {
	t.Helper()
	orig := pidAlive
	set := map[int]bool{}
	for _, p := range alive {
		set[p] = true
	}
	pidAlive = func(pid int) bool { return set[pid] }
	t.Cleanup(func() { pidAlive = orig })
}

// writeFakeSession writes a <pid>.json file into dir.
func writeFakeSession(t *testing.T, dir string, s sessionInfo) {
	t.Helper()
	b, err := json.Marshal(s)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", s.PID)), b, 0o600))
}

func TestValidateAddr(t *testing.T) {
	pass := []struct{ in, wantPort string }{
		{"127.0.0.1:8080", "8080"},
		{"[::1]:8080", "8080"},
		{"localhost:8080", "8080"},
		{"127.0.0.1:1", "1"},
		{"127.0.0.1:65535", "65535"},
		// Atoi accepts leading zeros and "+"; validateAddr returns the
		// CANONICAL port so downstream parsers can't disagree with it.
		{"127.0.0.1:080", "80"},
		{"127.0.0.1:+80", "80"},
	}
	for _, c := range pass {
		t.Run("ok/"+c.in, func(t *testing.T) {
			h, p, err := validateAddr(c.in)
			assert.NoError(t, err)
			assert.NotEmpty(t, h)
			assert.Equal(t, c.wantPort, p)
		})
	}
	fail := []string{
		"evil.com:80",
		"0.0.0.0:8080",
		"[::]:8080",
		"127.0.0.1:80@evil.com",
		"127.0.0.1:0",
		"127.0.0.1:99999",
		"127.0.0.1",  // no port
		"localhost",  // no port
		":8080",      // empty host (not loopback)
		"127.0.0.1:", // empty port
		"127.0.0.1:-1",
		"http://127.0.0.1:8080", // scheme not allowed
		// Loopback aliases that aren't on the exact-string allowlist.
		"LOCALHOST:80",          // case-sensitive
		"localhost.:80",         // FQDN dot
		"0177.0.0.1:80",         // octal
		"127.0.0.001:80",        // padded octets
		"127.1:80",              // short form
		"[::ffff:127.0.0.1]:80", // IPv4-mapped IPv6
	}
	for _, a := range fail {
		t.Run("rej/"+a, func(t *testing.T) {
			_, _, err := validateAddr(a)
			assert.Error(t, err)
		})
	}
}

func TestContainsPath(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		name    string
		repoDir string
		cwd     string
		want    bool
	}{
		{"exact", "/a/b", "/a/b", true},
		{"subdir", "/a/b", "/a/b/c", true},
		{"deep subdir", "/a/b", "/a/b/c/d/e", true},
		{"sibling", "/a/b", "/a/c", false},
		{"parent", "/a/b", "/a", false},
		{"dotdot-name inside", "/a", "/a/..foo", true},
		{"prefix not nested", "/a/foo", "/a/foobar", false},
		{"prefix not nested rev", "/a/foobar", "/a/foo", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Normalize separators for cross-platform — the function takes
			// already-cleaned, already-resolved paths.
			repo := strings.ReplaceAll(c.repoDir, "/", sep)
			cwd := strings.ReplaceAll(c.cwd, "/", sep)
			assert.Equal(t, c.want, containsPath(repo, cwd))
		})
	}
}

func TestContainsPathSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks unreliable on Windows")
	}
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	require.NoError(t, os.MkdirAll(filepath.Join(real, "sub"), 0o755))
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	// Resolve both as the discovery code does, then assert containment.
	resolvedRepo, err := filepath.EvalSymlinks(real)
	require.NoError(t, err)
	resolvedCwd, err := filepath.EvalSymlinks(filepath.Join(link, "sub"))
	require.NoError(t, err)
	assert.True(t, containsPath(resolvedRepo, resolvedCwd))
}

func TestReadSessionsSizeCap(t *testing.T) {
	dir := t.TempDir()
	// Legitimate small file.
	writeFakeSession(t, dir, sessionInfo{PID: 100, Addr: "127.0.0.1:1", RepoDir: "/a", Mode: "local"})
	// Oversized file (> maxSessionFileSize) — must be skipped before parse.
	big := append([]byte(`{"pid":200,"addr":"127.0.0.1:2","repo_dir":"/b","mode":"local"`), strings.Repeat(" ", maxSessionFileSize+1)...)
	big = append(big, '}')
	require.NoError(t, os.WriteFile(filepath.Join(dir, "200.json"), big, 0o600))
	// Unparseable file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "300.json"), []byte("not json"), 0o600))
	// Schema-invalid (no pid).
	require.NoError(t, os.WriteFile(filepath.Join(dir, "400.json"), []byte(`{"addr":"127.0.0.1:4"}`), 0o600))
	// Non-json file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "garbage.txt"), []byte("x"), 0o600))

	out, err := readSessions(dir)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, 100, out[0].PID)
}

// An old-format file (pre version/tabs) must still parse, with Tabs
// normalized to an empty slice so `sessions --json` prints [] not null.
func TestReadSessionsOldFormat(t *testing.T) {
	dir := t.TempDir()
	old := `{"pid":7,"addr":"127.0.0.1:9","port":9,"repo_dir":"/a","mode":"local","started_at":1}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "7.json"), []byte(old), 0o600))
	out, err := readSessions(dir)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "", out[0].Version)
	assert.NotNil(t, out[0].Tabs)
	assert.Empty(t, out[0].Tabs)
}

func TestReadSessionsMissingDir(t *testing.T) {
	out, err := readSessions(filepath.Join(t.TempDir(), "nonexistent"))
	require.NoError(t, err)
	assert.NotNil(t, out)
	assert.Empty(t, out)
}

func TestDiscoverSession(t *testing.T) {
	// Real directories under tmp so EvalSymlinks succeeds.
	tmp := t.TempDir()
	repoA := filepath.Join(tmp, "a")
	repoAB := filepath.Join(tmp, "a", "b")
	repoC := filepath.Join(tmp, "c")
	for _, d := range []string{repoAB, repoC} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}

	t.Run("simple match", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "local"})
		stubPidAlive(t, 10)
		got, _, err := discoverSession(dir, repoA)
		require.NoError(t, err)
		assert.Equal(t, 10, got.PID)
	})

	t.Run("subdir match", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "local"})
		stubPidAlive(t, 10)
		got, _, err := discoverSession(dir, repoAB)
		require.NoError(t, err)
		assert.Equal(t, 10, got.PID)
	})

	t.Run("relative repoPath is absolutized before matching", func(t *testing.T) {
		// EvalSymlinks of a relative path returns a relative result without
		// erroring, so an Abs-as-fallback never fires. Abs must come FIRST.
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "local"})
		stubPidAlive(t, 10)
		t.Chdir(repoAB)
		got, _, err := discoverSession(dir, ".")
		require.NoError(t, err)
		assert.Equal(t, 10, got.PID)
	})

	t.Run("most specific wins", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "local"})
		writeFakeSession(t, dir, sessionInfo{PID: 11, Addr: "127.0.0.1:5002", RepoDir: repoAB, Mode: "local"})
		stubPidAlive(t, 10, 11)
		got, _, err := discoverSession(dir, repoAB)
		require.NoError(t, err)
		assert.Equal(t, 11, got.PID, "deeper RepoDir should win")
	})

	t.Run("dead pid filtered", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "local"})
		stubPidAlive(t) // all dead
		_, _, err := discoverSession(dir, repoA)
		assert.Error(t, err)
		assert.NotContains(t, err.Error(), "5001", "dead session should not be listed")
	})

	t.Run("ssh mode filtered", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "ssh"})
		stubPidAlive(t, 10)
		_, _, err := discoverSession(dir, repoA)
		require.Error(t, err)
		// SSH session should still appear in the alive list of the error.
		assert.Contains(t, err.Error(), "127.0.0.1:5001")
		assert.Contains(t, err.Error(), "ssh")
	})

	t.Run("bad addr filtered", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "evil.com:80", RepoDir: repoA, Mode: "local"})
		stubPidAlive(t, 10)
		_, _, err := discoverSession(dir, repoA)
		assert.Error(t, err)
	})

	t.Run("root RepoDir filtered", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: "/", Mode: "local"})
		writeFakeSession(t, dir, sessionInfo{PID: 11, Addr: "127.0.0.1:5002", RepoDir: "//", Mode: "local"})
		stubPidAlive(t, 10, 11)
		_, _, err := discoverSession(dir, repoA)
		assert.Error(t, err)
	})

	t.Run("relative RepoDir filtered", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: "relative/path", Mode: "local"})
		stubPidAlive(t, 10)
		_, _, err := discoverSession(dir, repoA)
		assert.Error(t, err)
	})

	t.Run("symlink to root filtered after resolve", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks unreliable on Windows")
		}
		linkDir := t.TempDir()
		rootLink := filepath.Join(linkDir, "rootlink")
		if err := os.Symlink("/", rootLink); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: rootLink, Mode: "local"})
		stubPidAlive(t, 10)
		_, _, err := discoverSession(dir, repoA)
		assert.Error(t, err)
	})

	t.Run("tie on same RepoDir errors", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "local"})
		writeFakeSession(t, dir, sessionInfo{PID: 11, Addr: "127.0.0.1:5002", RepoDir: repoA, Mode: "local"})
		stubPidAlive(t, 10, 11)
		_, _, err := discoverSession(dir, repoA)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "multiple")
		assert.Contains(t, err.Error(), "5001")
		assert.Contains(t, err.Error(), "5002")
	})

	t.Run("sibling no match", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "local"})
		stubPidAlive(t, 10)
		_, _, err := discoverSession(dir, repoC)
		assert.Error(t, err)
	})

	t.Run("symlinked cwd resolves", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks unreliable on Windows")
		}
		linkBase := t.TempDir()
		link := filepath.Join(linkBase, "link")
		if err := os.Symlink(repoAB, link); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "local"})
		stubPidAlive(t, 10)
		got, _, err := discoverSession(dir, link)
		require.NoError(t, err)
		assert.Equal(t, 10, got.PID)
	})

	t.Run("zero matches lists alive sessions", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoC, Mode: "local"})
		writeFakeSession(t, dir, sessionInfo{PID: 11, Addr: "127.0.0.1:5002", RepoDir: "/remote/path", Mode: "ssh"})
		stubPidAlive(t, 10, 11)
		_, _, err := discoverSession(dir, repoA)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "127.0.0.1:5001")
		assert.Contains(t, err.Error(), "127.0.0.1:5002")
		assert.Contains(t, err.Error(), "--addr")
	})

	// --- multi-tab discovery ---

	t.Run("old-format session (no tabs) matches via RepoDir as tab 0", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "local"})
		stubPidAlive(t, 10)
		got, tab, err := discoverSession(dir, repoAB)
		require.NoError(t, err)
		assert.Equal(t, 10, got.PID)
		assert.Equal(t, sessionTab{ID: "0", Path: repoA}, tab)
	})

	t.Run("non-launch tab is discoverable", func(t *testing.T) {
		// Launched in repoC, repoA opened as tab 2: cwd in repoA/b must find
		// this instance (the v1 known gap) and name tab 2.
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoC, Mode: "local",
			Tabs: []sessionTab{{ID: "0", Path: repoC}, {ID: "2", Path: repoA}}})
		stubPidAlive(t, 10)
		got, tab, err := discoverSession(dir, repoAB)
		require.NoError(t, err)
		assert.Equal(t, 10, got.PID)
		assert.Equal(t, "2", tab.ID)
		assert.Equal(t, repoA, tab.Path)
	})

	t.Run("nested tabs in one session: longest path wins", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "local",
			Tabs: []sessionTab{{ID: "0", Path: repoA}, {ID: "1", Path: repoAB}, {ID: "3", Path: repoC}}})
		stubPidAlive(t, 10)
		_, tab, err := discoverSession(dir, repoAB)
		require.NoError(t, err)
		assert.Equal(t, "1", tab.ID, "cwd inside both /a and /a/b must target the inner tab")
		_, tab, err = discoverSession(dir, repoA)
		require.NoError(t, err)
		assert.Equal(t, "0", tab.ID)
	})

	t.Run("deeper tab in another session beats shallower launch repo", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "local",
			Tabs: []sessionTab{{ID: "0", Path: repoA}}})
		writeFakeSession(t, dir, sessionInfo{PID: 11, Addr: "127.0.0.1:5002", RepoDir: repoC, Mode: "local",
			Tabs: []sessionTab{{ID: "0", Path: repoC}, {ID: "4", Path: repoAB}}})
		stubPidAlive(t, 10, 11)
		got, tab, err := discoverSession(dir, repoAB)
		require.NoError(t, err)
		assert.Equal(t, 11, got.PID)
		assert.Equal(t, "4", tab.ID)
	})

	t.Run("same repo open in two sessions via tabs is still a tie", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoA, Mode: "local",
			Tabs: []sessionTab{{ID: "0", Path: repoA}}})
		writeFakeSession(t, dir, sessionInfo{PID: 11, Addr: "127.0.0.1:5002", RepoDir: repoC, Mode: "local",
			Tabs: []sessionTab{{ID: "0", Path: repoC}, {ID: "1", Path: repoA}}})
		stubPidAlive(t, 10, 11)
		_, _, err := discoverSession(dir, repoA)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "multiple")
		assert.Contains(t, err.Error(), "tab 1")
	})

	t.Run("ssh session tabs never match even if the path exists locally", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: "/remote", Mode: "ssh",
			Tabs: []sessionTab{{ID: "0", Path: "/remote"}, {ID: "1", Path: repoA}}})
		stubPidAlive(t, 10)
		_, _, err := discoverSession(dir, repoA)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tab 1", "zero-match listing shows extra tabs")
	})

	t.Run("non-numeric tab id is dropped", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoC, Mode: "local",
			Tabs: []sessionTab{{ID: "0", Path: repoC}, {ID: "../x", Path: repoA}}})
		stubPidAlive(t, 10)
		_, _, err := discoverSession(dir, repoA)
		assert.Error(t, err)
	})

	t.Run("root tab path filtered, session survives via other tabs", func(t *testing.T) {
		dir := t.TempDir()
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: repoC, Mode: "local",
			Tabs: []sessionTab{{ID: "0", Path: repoC}, {ID: "1", Path: "/"}, {ID: "2", Path: repoA}}})
		stubPidAlive(t, 10)
		_, tab, err := discoverSession(dir, repoAB)
		require.NoError(t, err)
		assert.Equal(t, "2", tab.ID, `"/" tab must not universally match`)
	})
}

func TestResolveTabPath(t *testing.T) {
	tab2 := sessionTab{ID: "2", Path: "/x"}
	cases := []struct {
		name, in string
		tab      sessionTab
		want     string
	}{
		{"tab-relative rewritten", "/api/log", tab2, "/tab/2/api/log"},
		{"query preserved", "/api/file-show?revision=@&path=a", tab2, "/tab/2/api/file-show?revision=@&path=a"},
		{"bare /api index", "/api", tab2, "/tab/2/api"},
		{"bare /api with query", "/api?x=1", tab2, "/tab/2/api?x=1"},
		{"tab 0 also rewritten (kills the SPA-HTML footgun)", "/api/log", sessionTab{ID: "0"}, "/tab/0/api/log"},
		{"explicit tab left alone", "/tab/0/api/log", tab2, "/tab/0/api/log"},
		{"root route left alone", "/tabs", tab2, "/tabs"},
		{"lookalike prefix left alone", "/apix/log", tab2, "/apix/log"},
		{"no tab (e.g. --addr) → verbatim", "/api/log", sessionTab{}, "/api/log"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, resolveTabPath(c.in, c.tab))
		})
	}
}

func TestExplicitTabID(t *testing.T) {
	assert.Equal(t, "3", explicitTabID("/tab/3/api/log"))
	assert.Equal(t, "3", explicitTabID("/tab/3"))
	assert.Equal(t, "3", explicitTabID("/tab/3?x"))
	assert.Equal(t, "", explicitTabID("/api/log"))
	assert.Equal(t, "", explicitTabID("/tabs"))
}

func TestVersionMismatchWarning(t *testing.T) {
	assert.Empty(t, versionMismatchWarning(sessionInfo{PID: 1, Version: ""}, "1.0.0"), "older server without a stamp → silent")
	assert.Empty(t, versionMismatchWarning(sessionInfo{PID: 1, Version: "1.0.0"}, "1.0.0"))
	w := versionMismatchWarning(sessionInfo{PID: 42, Version: "1.0.0"}, "1.1.0")
	assert.Contains(t, w, "warning")
	assert.Contains(t, w, "pid 42")
	assert.Contains(t, w, "1.0.0")
	assert.Contains(t, w, "1.1.0")

	// Only release-number differences are the stale-binary class. Dev builds
	// (go run → "dev"), VCS pseudo-versions, +dirty metadata and pre-release
	// suffixes must not warn — the two-terminal dev loop pairs a go-run
	// server with a built CLI on every call.
	assert.Empty(t, versionMismatchWarning(sessionInfo{PID: 1, Version: "dev"}, "1.36.1"))
	assert.Empty(t, versionMismatchWarning(sessionInfo{PID: 1, Version: "1.36.1"}, "dev"))
	assert.Empty(t, versionMismatchWarning(sessionInfo{PID: 1, Version: "1.36.1+dirty"}, "1.36.1"))
	assert.Empty(t, versionMismatchWarning(sessionInfo{PID: 1, Version: "1.36.2-0.20260811123456-abcdef123456"}, "1.36.1"))
	assert.Empty(t, versionMismatchWarning(sessionInfo{PID: 1, Version: "1.37.0-rc1"}, "v1.37.0"))
	assert.NotEmpty(t, versionMismatchWarning(sessionInfo{PID: 1, Version: "1.36.1+dirty"}, "1.37.0"))
}

func TestReleaseCore(t *testing.T) {
	for in, want := range map[string]string{
		"1.2.3": "1.2.3", "v1.2.3": "1.2.3", "1.2.3+dirty": "1.2.3", "1.2.3-rc1": "1.2.3",
		"dev": "", "": "", "1.2": "", "1.2.3.4": "", "1.x.3": "",
		"1.36.2-0.20260811123456-abcdef123456": "",
	} {
		assert.Equal(t, want, releaseCore(in), in)
	}
}

// captureStderr swaps os.Stderr for a pipe around fn and returns what was
// written. runAPISubcommand prints straight to os.Stderr (it IS the CLI).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stderr
	os.Stderr = w
	done := make(chan []byte)
	go func() { b, _ := io.ReadAll(r); done <- b }()
	fn()
	os.Stderr = orig
	w.Close()
	return string(<-done)
}

// TestRunAPISubcommandMultiTab drives the discovery → tab targeting → request
// pipeline through runAPISubcommand itself: session dir via XDG_RUNTIME_DIR,
// a real loopback server recording the path it was hit on, stderr captured.
func TestRunAPISubcommandMultiTab(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("session files are no-op on Windows")
	}
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		w.WriteHeader(200)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", base)
	dir := filepath.Join(base, "lightjj", "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	launch := filepath.Join(t.TempDir(), "launch")
	other := filepath.Join(t.TempDir(), "other")
	otherSub := filepath.Join(other, "src")
	for _, d := range []string{launch, otherSub} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	stubPidAlive(t, 10)
	write := func(version string) {
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: addr, RepoDir: launch, Mode: "local", Version: version,
			Tabs: []sessionTab{{ID: "0", Path: launch}, {ID: "2", Path: other}}})
	}
	write(resolvedVersion())

	t.Run("cwd in non-launch tab: /api/ rewritten to /tab/2/, announced on stderr", func(t *testing.T) {
		var code int
		stderr := captureStderr(t, func() {
			code = runAPISubcommand([]string{"--repo", otherSub, "GET", "/api/log?limit=5"})
		})
		assert.Equal(t, 0, code)
		assert.Equal(t, "/tab/2/api/log?limit=5", gotPath)
		assert.Contains(t, stderr, "tab 2 ("+other+")")
		assert.Contains(t, stderr, "→ /tab/2/api/log?limit=5")
		assert.NotContains(t, stderr, "warning", "matching version → no stale-binary warning")
	})

	t.Run("explicit /tab/0/ honoured verbatim but flagged", func(t *testing.T) {
		stderr := captureStderr(t, func() {
			require.Equal(t, 0, runAPISubcommand([]string{"--repo", otherSub, "GET", "/tab/0/api/log"}))
		})
		assert.Equal(t, "/tab/0/api/log", gotPath)
		assert.Contains(t, stderr, "targets /tab/0/ explicitly")
	})

	t.Run("cwd in launch tab: rewritten to /tab/0/, silent", func(t *testing.T) {
		stderr := captureStderr(t, func() {
			require.Equal(t, 0, runAPISubcommand([]string{"--repo", launch, "GET", "/api/log"}))
		})
		assert.Equal(t, "/tab/0/api/log", gotPath)
		assert.Empty(t, stderr)
	})

	t.Run("--addr bypasses discovery: path verbatim, no tab logic", func(t *testing.T) {
		stderr := captureStderr(t, func() {
			require.Equal(t, 0, runAPISubcommand([]string{"--addr", addr, "GET", "/api/log"}))
		})
		assert.Equal(t, "/api/log", gotPath)
		assert.Empty(t, stderr)
	})

	t.Run("version mismatch warns on stderr, request still sent", func(t *testing.T) {
		// Under `go test` resolvedVersion() is "dev" (never warns by design), so
		// pin a release version for this binary via the ldflags var.
		version = "9.8.7"
		t.Cleanup(func() { version = "" })
		write("0.0.1")
		var code int
		stderr := captureStderr(t, func() {
			code = runAPISubcommand([]string{"--repo", launch, "GET", "/api/log"})
		})
		assert.Equal(t, 0, code)
		assert.Equal(t, "/tab/0/api/log", gotPath)
		assert.Contains(t, stderr, "warning")
		assert.Contains(t, stderr, "0.0.1")
		assert.Contains(t, stderr, "9.8.7")
	})

	t.Run("PATH must start with exactly one slash", func(t *testing.T) {
		gotPath = ""
		for _, bad := range []string{"api/log", "//api/log"} {
			var code int
			stderr := captureStderr(t, func() {
				code = runAPISubcommand([]string{"--repo", launch, "GET", bad})
			})
			assert.Equal(t, 2, code, bad)
			assert.Contains(t, stderr, "single '/'", bad)
		}
		assert.Empty(t, gotPath, "nothing sent")
	})

	t.Run("--addr with --repo notes that --repo is ignored", func(t *testing.T) {
		stderr := captureStderr(t, func() {
			require.Equal(t, 0, runAPISubcommand([]string{"--addr", addr, "--repo", launch, "GET", "/tab/0/api/log"}))
		})
		assert.Equal(t, "/tab/0/api/log", gotPath)
		assert.Contains(t, stderr, "--repo")
	})

	t.Run("old-format session: no warning, /api/ → /tab/0/", func(t *testing.T) {
		old := fmt.Sprintf(`{"pid":10,"addr":%q,"repo_dir":%q,"mode":"local","started_at":1}`, addr, launch)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "10.json"), []byte(old), 0o600))
		stderr := captureStderr(t, func() {
			require.Equal(t, 0, runAPISubcommand([]string{"--repo", launch, "GET", "/api/log"}))
		})
		assert.Equal(t, "/tab/0/api/log", gotPath)
		assert.Empty(t, stderr)
	})
}

func TestRunSessionsSubcommandShowsVersionAndTabs(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", base)
	dir := filepath.Join(base, "lightjj", "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: "/a", Mode: "local", Version: "1.2.3",
		Tabs: []sessionTab{{ID: "0", Path: "/a"}, {ID: "1", Path: "/b"}}})
	old := `{"pid":11,"addr":"127.0.0.1:5002","repo_dir":"/c","mode":"local","started_at":1}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "11.json"), []byte(old), 0o600))
	stubPidAlive(t, 10, 11)

	out := captureStdout(t, func() { require.Equal(t, 0, runSessionsSubcommand(nil)) })
	assert.Contains(t, out, "VERSION")
	assert.Contains(t, out, "1.2.3")
	assert.Contains(t, out, "tab 1: /b")
	assert.NotContains(t, out, "tab 0", "tab 0 is the REPO column, not repeated")
	assert.Regexp(t, `11\s+127\.0\.0\.1:5002\s+local\s+-\s+/c`, out, "old-format session shows - for version")

	js := captureStdout(t, func() { require.Equal(t, 0, runSessionsSubcommand([]string{"--json"})) })
	var parsed []sessionInfo
	require.NoError(t, json.Unmarshal([]byte(js), &parsed))
	require.Len(t, parsed, 2)
	assert.Equal(t, "1.2.3", parsed[0].Version)
	assert.Len(t, parsed[0].Tabs, 2)
	assert.Contains(t, js, `"tabs": []`, "old-format session serializes tabs as [] not null")
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stdout
	os.Stdout = w
	done := make(chan []byte)
	go func() { b, _ := io.ReadAll(r); done <- b }()
	fn()
	os.Stdout = orig
	w.Close()
	return string(<-done)
}

// echoHandler writes the request method, path, query, content-type, and body
// back as a JSON object so tests can assert on them.
func echoHandler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		out := map[string]string{
			"method":       r.Method,
			"path":         r.URL.Path,
			"query":        r.URL.RawQuery,
			"content_type": r.Header.Get("Content-Type"),
			"x_extra":      r.Header.Get("X-Extra"),
			"body":         string(body),
			"host":         r.Host,
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(out)
	}
}

func TestDoAPIRequest(t *testing.T) {
	srv := httptest.NewServer(echoHandler(200))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	t.Run("GET no body", func(t *testing.T) {
		resp, err := doAPIRequest(addr, "GET", "/api/log?revset=@", nil, nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		var got map[string]string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
		assert.Equal(t, "GET", got["method"])
		assert.Equal(t, "/api/log", got["path"])
		assert.Equal(t, "revset=@", got["query"])
		assert.Empty(t, got["content_type"], "no Content-Type without body")
		// Host must round-trip the validated addr unmodified — overriding it
		// to "localhost" would let the CLI lie to a server about its
		// destination and weaken DNS-rebinding protection.
		assert.Equal(t, addr, got["host"])
	})

	t.Run("POST with body sets Content-Type", func(t *testing.T) {
		resp, err := doAPIRequest(addr, "POST", "/api/abandon", strings.NewReader(`{"x":1}`), nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		var got map[string]string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
		assert.Equal(t, "POST", got["method"])
		assert.Equal(t, "application/json", got["content_type"])
		assert.Equal(t, `{"x":1}`, got["body"])
	})

	t.Run("custom Content-Type wins", func(t *testing.T) {
		resp, err := doAPIRequest(addr, "POST", "/api/x", strings.NewReader("a=b"),
			[]string{"Content-Type: text/plain"})
		require.NoError(t, err)
		defer resp.Body.Close()
		var got map[string]string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
		assert.Equal(t, "text/plain", got["content_type"])
	})

	t.Run("extra header passes through", func(t *testing.T) {
		resp, err := doAPIRequest(addr, "GET", "/api/x", nil, []string{"X-Extra: hello"})
		require.NoError(t, err)
		defer resp.Body.Close()
		var got map[string]string
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
		assert.Equal(t, "hello", got["x_extra"])
	})

	t.Run("rejects non-loopback addr", func(t *testing.T) {
		_, err := doAPIRequest("evil.com:80", "GET", "/", nil, nil)
		assert.Error(t, err)
	})

	t.Run("rejects bad header", func(t *testing.T) {
		_, err := doAPIRequest(addr, "GET", "/", nil, []string{"NoColonHeader"})
		assert.Error(t, err)
	})
}

func TestRunAPISubcommandExitCodes(t *testing.T) {
	t.Run("usage: no args", func(t *testing.T) {
		assert.Equal(t, 2, runAPISubcommand(nil))
	})
	t.Run("usage: one arg", func(t *testing.T) {
		assert.Equal(t, 2, runAPISubcommand([]string{"GET"}))
	})
	t.Run("usage: too many positionals", func(t *testing.T) {
		assert.Equal(t, 2, runAPISubcommand([]string{"GET", "/", "body", "extra"}))
	})
	t.Run("usage: bad flag", func(t *testing.T) {
		assert.Equal(t, 2, runAPISubcommand([]string{"--nope", "GET", "/"}))
	})
	t.Run("usage: bad addr", func(t *testing.T) {
		assert.Equal(t, 2, runAPISubcommand([]string{"--addr", "evil.com:80", "GET", "/"}))
	})

	statusCases := []struct {
		status int
		exit   int
	}{
		{200, 0},
		{201, 0},
		{204, 0},
		{301, 0},
		{400, 4},
		{404, 4},
		{422, 4},
		{500, 5},
		{502, 5},
	}
	for _, c := range statusCases {
		t.Run(fmt.Sprintf("status %d → exit %d", c.status, c.exit), func(t *testing.T) {
			srv := httptest.NewServer(echoHandler(c.status))
			defer srv.Close()
			addr := strings.TrimPrefix(srv.URL, "http://")
			got := runAPISubcommand([]string{"--addr", addr, "GET", "/api/x"})
			assert.Equal(t, c.exit, got)
		})
	}

	t.Run("connection refused → exit 1", func(t *testing.T) {
		// Reserve a port and close it so it's almost certainly free.
		srv := httptest.NewServer(echoHandler(200))
		addr := strings.TrimPrefix(srv.URL, "http://")
		srv.Close()
		got := runAPISubcommand([]string{"--addr", addr, "GET", "/api/x"})
		assert.Equal(t, 1, got)
	})

	t.Run("discovery: no session dir → exit 1", func(t *testing.T) {
		// Force the TempDir-fallback path by clearing XDG_RUNTIME_DIR and
		// pointing TMPDIR at an empty temp dir — sessionDirReadOnly's
		// verifyOwnedDir will return ENOENT.
		t.Setenv("XDG_RUNTIME_DIR", "")
		t.Setenv("TMPDIR", t.TempDir())
		got := runAPISubcommand([]string{"GET", "/api/x"})
		assert.Equal(t, 1, got)
	})

	t.Run("body @file missing → exit 2", func(t *testing.T) {
		srv := httptest.NewServer(echoHandler(200))
		defer srv.Close()
		addr := strings.TrimPrefix(srv.URL, "http://")
		got := runAPISubcommand([]string{"--addr", addr, "POST", "/api/x", "@" + filepath.Join(t.TempDir(), "nope.json")})
		assert.Equal(t, 2, got)
	})
}

func TestRunSessionsSubcommand(t *testing.T) {
	t.Run("no session dir → exit 0", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		t.Setenv("TMPDIR", t.TempDir())
		got := runSessionsSubcommand(nil)
		assert.Equal(t, 0, got)
	})

	t.Run("with sessions → exit 0", func(t *testing.T) {
		base := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", base)
		dir := filepath.Join(base, "lightjj", "sessions")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		writeFakeSession(t, dir, sessionInfo{PID: 10, Addr: "127.0.0.1:5001", RepoDir: "/a", Mode: "local"})
		writeFakeSession(t, dir, sessionInfo{PID: 11, Addr: "127.0.0.1:5002", RepoDir: "/b", Mode: "ssh"})
		stubPidAlive(t, 10, 11)
		got := runSessionsSubcommand(nil)
		assert.Equal(t, 0, got)
		got = runSessionsSubcommand([]string{"--json"})
		assert.Equal(t, 0, got)
	})
}

func TestSessionDirReadOnly(t *testing.T) {
	t.Run("ENOENT on TempDir fallback", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		t.Setenv("TMPDIR", t.TempDir())
		_, err := sessionDirReadOnly()
		require.Error(t, err)
		assert.True(t, errors.Is(err, os.ErrNotExist),
			"expected ENOENT-class error, got: %v", err)
	})

	t.Run("XDG path verifies unconditionally", func(t *testing.T) {
		// The reader verifies on the XDG path too — unlike the writer, which
		// trusts the spec's user-ownership guarantee. A stray
		// XDG_RUNTIME_DIR=/tmp would silently disable the only trust check
		// between sessions/<pid>.json content and the network.
		base := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", base)

		// Missing lightjj/ → ENOENT (mapped to "not running" by callers).
		_, err := sessionDirReadOnly()
		require.Error(t, err)
		assert.True(t, errors.Is(err, os.ErrNotExist), "expected ENOENT, got %v", err)

		// Present and 0700 → ok.
		require.NoError(t, os.MkdirAll(filepath.Join(base, "lightjj", "sessions"), 0o700))
		dir, err := sessionDirReadOnly()
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(base, "lightjj", "sessions"), dir)

		// Present but group/other readable → hard error, not ENOENT.
		require.NoError(t, os.Chmod(filepath.Join(base, "lightjj"), 0o755))
		_, err = sessionDirReadOnly()
		require.Error(t, err)
		assert.False(t, errors.Is(err, os.ErrNotExist), "perms error should not be ENOENT-class")
	})
}
