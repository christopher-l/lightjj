package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chronologos/lightjj/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionFileRoundTrip(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	info := sessionInfo{
		PID:       os.Getpid(),
		Addr:      "127.0.0.1:54321",
		Port:      54321,
		RepoDir:   "/home/user/repo",
		Mode:      "local",
		StartedAt: 1234567890,
		Version:   "1.2.3",
		Tabs: []sessionTab{
			{ID: "0", Path: "/home/user/repo"},
			{ID: "2", Path: "/home/user/other"},
		},
	}
	path := writeSessionFile(info)
	require.NotEmpty(t, path)
	t.Cleanup(func() { os.Remove(path) })

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var got sessionInfo
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, info, got)

	// Wire names are the documented contract (agent_api.md fallback section
	// tells agents to jq these keys) — pin them.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	assert.Equal(t, "1.2.3", raw["version"])
	tabs, ok := raw["tabs"].([]any)
	require.True(t, ok, "tabs must serialize as an array")
	require.Len(t, tabs, 2)
	assert.Equal(t, map[string]any{"id": "2", "path": "/home/user/other"}, tabs[1])

	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	// Atomic write leaves no temp siblings behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasSuffix(e.Name(), ".tmp"), "leftover temp file %s", e.Name())
	}
}

func TestSessionFileNilTabsSerializeAsArray(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path := writeSessionFile(sessionInfo{PID: os.Getpid(), Addr: "127.0.0.1:1", RepoDir: "/r", Mode: "local"})
	require.NotEmpty(t, path)
	t.Cleanup(func() { os.Remove(path) })
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"tabs":[]`)
	assert.NotContains(t, string(data), `"version"`, "empty version is omitted, not written as \"\"")
}

// TestSessionWriterUpdate covers the tab-list rewrite path main.go wires to
// TabManager.OnTabsChange: the snapshot func is re-invoked on every Update so
// the file tracks opens/closes, identity fields stay fixed, Remove cleans up.
func TestSessionWriterUpdate(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	tabs := []sessionTab{{ID: "0", Path: "/r"}}
	w := newSessionWriter(
		sessionInfo{PID: os.Getpid(), Addr: "127.0.0.1:7", RepoDir: "/r", Mode: "local", Version: "9.9.9"},
		func() []sessionTab { return tabs },
	)
	read := func() sessionInfo {
		t.Helper()
		require.NotEmpty(t, w.path)
		data, err := os.ReadFile(w.path)
		require.NoError(t, err)
		var s sessionInfo
		require.NoError(t, json.Unmarshal(data, &s))
		return s
	}

	w.Update()
	assert.Equal(t, tabs, read().Tabs)

	tabs = append(tabs, sessionTab{ID: "1", Path: "/other"}) // tab opened
	w.Update()
	got := read()
	assert.Equal(t, tabs, got.Tabs)
	assert.Equal(t, "9.9.9", got.Version)
	assert.Equal(t, "/r", got.RepoDir, "identity fields are fixed across updates")

	tabs = tabs[:1] // tab closed
	w.Update()
	assert.Equal(t, tabs, read().Tabs)

	path := w.path
	w.Remove()
	assert.NoFileExists(t, path)

	// Shutdown latch: a tab handler racing the signal must not resurrect the
	// file for a pid that is about to exit.
	w.Update()
	assert.NoFileExists(t, path)
}

func TestSessionWriterConcurrentUpdates(t *testing.T) {
	// Concurrent open/close notifications must never leave a torn or
	// half-written file: every Update is snapshot+atomic-write under mu.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	var mu sync.Mutex
	tabs := []sessionTab{{ID: "0", Path: "/r"}}
	w := newSessionWriter(
		sessionInfo{PID: os.Getpid(), Addr: "127.0.0.1:7", RepoDir: "/r", Mode: "local"},
		func() []sessionTab { mu.Lock(); defer mu.Unlock(); return append([]sessionTab{}, tabs...) },
	)
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mu.Lock()
			tabs = append(tabs, sessionTab{ID: fmt.Sprint(i), Path: fmt.Sprintf("/r%d", i)})
			mu.Unlock()
			w.Update()
		}(i)
	}
	wg.Wait()
	t.Cleanup(w.Remove)
	data, err := os.ReadFile(w.path)
	require.NoError(t, err)
	var got sessionInfo
	require.NoError(t, json.Unmarshal(data, &got), "file must always be complete JSON")
	// The last Update to take w.mu snapshotted after every append it raced
	// with had landed → the final file holds all 21 tabs.
	assert.Len(t, got.Tabs, 21)
}

func TestSessionTabsOf(t *testing.T) {
	got := sessionTabsOf([]api.TabRef{{ID: "0", Path: "/a"}, {ID: "3", Path: "/b"}})
	assert.Equal(t, []sessionTab{{ID: "0", Path: "/a"}, {ID: "3", Path: "/b"}}, got)
	assert.Equal(t, []sessionTab{}, sessionTabsOf(nil), "empty slice, not nil")
}

func TestSweepStaleSessions(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", tmp)
	dir, err := sessionDir()
	require.NoError(t, err)

	live := filepath.Join(dir, fmt.Sprintf("%d.json", os.Getpid()))
	require.NoError(t, os.WriteFile(live, []byte("{}"), 0o600))
	// PID 1 init/launchd is always alive on Unix; pick something guaranteed
	// dead by writing our own pid +1e6 (well beyond pid_max).
	dead := filepath.Join(dir, "999999999.json")
	require.NoError(t, os.WriteFile(dead, []byte("{}"), 0o600))
	notJSON := filepath.Join(dir, "garbage.txt")
	require.NoError(t, os.WriteFile(notJSON, []byte("x"), 0o600))

	sweepStaleSessions(dir)

	assert.FileExists(t, live, "live pid file should survive")
	assert.NoFileExists(t, dead, "dead pid file should be removed")
	assert.FileExists(t, notJSON, "non-json files should be ignored")
}

func TestPidAlive(t *testing.T) {
	assert.True(t, pidAlive(os.Getpid()))
	assert.False(t, pidAlive(999999999))
}

func TestVerifyOwnedDir(t *testing.T) {
	tmp := t.TempDir()

	tight := filepath.Join(tmp, "tight")
	require.NoError(t, os.Mkdir(tight, 0o700))
	require.NoError(t, os.Chmod(tight, 0o700)) // umask may have widened it
	assert.NoError(t, verifyOwnedDir(tight))

	loose := filepath.Join(tmp, "loose")
	require.NoError(t, os.Mkdir(loose, 0o777))
	require.NoError(t, os.Chmod(loose, 0o755))
	assert.ErrorContains(t, verifyOwnedDir(loose), "group/other")

	link := filepath.Join(tmp, "link")
	require.NoError(t, os.Symlink(tight, link))
	assert.ErrorContains(t, verifyOwnedDir(link), "not a plain directory")
}
