package main

import (
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

// TestAPIPipelineRoundTrip wires discovery → request building end-to-end
// against a real HTTP server. The unit tests cover discoverSession and
// doAPIRequest in isolation; this is the test that catches "the two stages
// disagree on what an addr / path looks like" — e.g. discovery returning a
// `[::1]:N` form that validateAddr rejects, or url.Parse mishandling the
// query string a real route uses.
func TestAPIPipelineRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("session files are no-op on Windows")
	}

	// Real HTTP server. httptest.NewServer binds 127.0.0.1:<random> so the
	// discovered addr passes validateAddr without any test stubbing.
	type seen struct {
		method, path, query, contentType, body string
	}
	var got seen
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = seen{
			method:      r.Method,
			path:        r.URL.Path,
			query:       r.URL.RawQuery,
			contentType: r.Header.Get("Content-Type"),
			body:        string(b),
		}
		// Echo the body back so the assertion proves a *round trip*, not just
		// that the server was reached.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://") // "127.0.0.1:NNNNN"

	// Session file in a temp dir, repo dir is a temp dir the test cwd is "in".
	sessDir := t.TempDir()
	repoDir := t.TempDir()
	cwd := filepath.Join(repoDir, "src")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	stubPidAlive(t, 100)
	writeFakeSession(t, sessDir, sessionInfo{
		PID: 100, Addr: addr, Mode: "local", RepoDir: repoDir,
	})

	t.Run("POST with JSON body and query string", func(t *testing.T) {
		sess, _, err := discoverSession(sessDir, cwd)
		require.NoError(t, err)
		require.Equal(t, addr, sess.Addr, "discovery must return the listener's addr verbatim")

		body := `{"changeId":"abc","comment":"review me"}`
		resp, err := doAPIRequest(sess.Addr, "POST", "/tab/0/api/annotations?changeId=abc", strings.NewReader(body), nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "POST", got.method)
		assert.Equal(t, "/tab/0/api/annotations", got.path, "path must survive url.Parse split")
		assert.Equal(t, "changeId=abc", got.query, "query string must survive url.Parse split")
		assert.Equal(t, "application/json", got.contentType, "Content-Type defaulted when body present")
		assert.Equal(t, body, got.body, "body must arrive byte-for-byte")
		assert.Equal(t, body, string(respBody), "response body must round-trip")
	})

	t.Run("GET with no body has no default Content-Type", func(t *testing.T) {
		sess, _, err := discoverSession(sessDir, cwd)
		require.NoError(t, err)
		resp, err := doAPIRequest(sess.Addr, "GET", "/tab/0/api/focus", nil, nil)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, "GET", got.method)
		assert.Empty(t, got.contentType, "no body → no default Content-Type")
	})

	t.Run("custom -H header overrides default Content-Type", func(t *testing.T) {
		sess, _, err := discoverSession(sessDir, cwd)
		require.NoError(t, err)
		resp, err := doAPIRequest(sess.Addr, "POST", "/api/x", strings.NewReader("raw"),
			[]string{"Content-Type: text/plain"})
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, "text/plain", got.contentType,
			"explicit -H Content-Type must suppress the application/json default")
	})
}

// TestAPIPipelineStatusMapping proves the discovered server's response status
// reaches the caller untouched (no error-wrapping that would coerce a 4xx into
// a Go error). The status→exit-code mapping itself lives in runAPISubcommand
// and is covered by api_cmd_test.go's exit-code table; this test guards the
// step BEFORE it — that doAPIRequest doesn't eat the status.
func TestAPIPipelineStatusMapping(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("session files are no-op on Windows")
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/notfound":
			w.WriteHeader(http.StatusNotFound)
		case "/oops":
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	sessDir := t.TempDir()
	repoDir := t.TempDir()
	stubPidAlive(t, 200)
	writeFakeSession(t, sessDir, sessionInfo{
		PID: 200, Addr: addr, Mode: "local", RepoDir: repoDir,
	})
	sess, _, err := discoverSession(sessDir, repoDir)
	require.NoError(t, err)

	cases := []struct {
		path     string
		wantCode int
	}{
		{"/ok", http.StatusOK},
		{"/notfound", http.StatusNotFound},
		{"/oops", http.StatusInternalServerError},
	}
	for _, c := range cases {
		resp, err := doAPIRequest(sess.Addr, "GET", c.path, nil, nil)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, c.wantCode, resp.StatusCode,
			"discovered-server response status must reach the exit-code mapper untouched")
	}
}
