package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMMDSClientReadsMetadata(t *testing.T) {
	var attempts atomic.Int32
	var accept atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest/api/token":
			if r.Method != http.MethodPut {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			// Fail the first attempt to exercise the retry loop.
			if attempts.Add(1) < 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("mmds-token"))
		case "/latest/meta-data/":
			accept.Store(r.Header.Get("Accept"))
			if r.Header.Get("X-metadata-token") != "mmds-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(metadata{
				JobID:      42,
				Repository: "owner/repo",
				RunnerName: "runner-1",
				JITConfig:  "encoded",
				DNS:        []string{"1.1.1.1"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	meta, err := newMMDSClient(srv.URL).waitForMetadata(ctx)
	require.NoError(t, err)
	assert.Equal(t, "application/json", accept.Load())
	assert.Equal(t, int64(42), meta.JobID)
	assert.Equal(t, "runner-1", meta.RunnerName)
	assert.Equal(t, "encoded", meta.JITConfig)
}

func TestMMDSClientTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not ready", http.StatusNotFound)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := newMMDSClient(srv.URL).waitForMetadata(ctx)
	require.Error(t, err)
}

func TestEnsureHostsEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	require.NoError(t, os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o644))

	require.NoError(t, ensureHostsEntry(path, "runner-1"))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(data), "runner-1"))

	// A second call must not duplicate the entry.
	require.NoError(t, ensureHostsEntry(path, "runner-1"))
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(data), "runner-1"))
}

func TestWriteResolvConfTo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))

	require.NoError(t, writeResolvConfTo(path, []string{"1.1.1.1", "bogus", "8.8.8.8"}))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", string(got))

	// No nameservers leaves the existing file untouched.
	require.NoError(t, writeResolvConfTo(path, nil))
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", string(got))
}

func TestLoadEnvDefaults(t *testing.T) {
	t.Setenv("FCGHACT_MMDS_BASE", "")
	t.Setenv("FCGHACT_MMDS_TIMEOUT", "42s")
	cfg := loadEnv()
	assert.Equal(t, defaultMMDSBase, cfg.mmdsBase)
	assert.Equal(t, 42*time.Second, cfg.waitTimeout)
}
