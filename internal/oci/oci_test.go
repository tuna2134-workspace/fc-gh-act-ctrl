package oci

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestPushPullRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "rootfs.ext4")
	content := bytes.Repeat([]byte("fcghact-rootfs-"), 4096)
	require.NoError(t, os.WriteFile(src, content, 0o644))

	host := testRegistry(t)
	ref := host + "/fcghact/rootfs:test"
	ctx := context.Background()
	opts := Options{Reference: ref, Insecure: true, TempDir: t.TempDir(), Platform: "amd64"}

	pushed, err := Push(ctx, src, opts)
	require.NoError(t, err)
	assert.NotEmpty(t, pushed.ImageDigest)
	assert.NotEmpty(t, pushed.LayerDigest)

	cacheDir := t.TempDir()
	pullOpts := opts
	pullOpts.CacheDir = cacheDir
	pulled, err := Pull(ctx, pullOpts)
	require.NoError(t, err)
	assert.False(t, pulled.Cached)
	assert.Equal(t, pushed.ImageDigest, pulled.ImageDigest)
	assert.Equal(t, pushed.LayerDigest, pulled.LayerDigest)

	got, err := os.ReadFile(pulled.Path)
	require.NoError(t, err)
	assert.Equal(t, content, got)

	// The second pull must reuse the cache.
	again, err := Pull(ctx, pullOpts)
	require.NoError(t, err)
	assert.True(t, again.Cached)

	info, err := Inspect(ctx, pullOpts)
	require.NoError(t, err)
	assert.True(t, info.Cached)
	assert.Equal(t, pushed.ImageDigest, info.ImageDigest)
}

func TestPruneKeepsCurrentLayer(t *testing.T) {
	dir := t.TempDir()
	host := testRegistry(t)
	ctx := context.Background()

	write := func(name string, content []byte) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, content, 0o644))
		return path
	}
	opts := Options{Reference: host + "/fcghact/rootfs:one", Insecure: true, TempDir: t.TempDir()}
	first, err := Push(ctx, write("first.ext4", []byte("first-rootfs")), opts)
	require.NoError(t, err)

	cacheDir := t.TempDir()
	pullOpts := opts
	pullOpts.CacheDir = cacheDir
	_, err = Pull(ctx, pullOpts)
	require.NoError(t, err)

	opts.Reference = host + "/fcghact/rootfs:two"
	second, err := Push(ctx, write("second.ext4", []byte("second-rootfs")), opts)
	require.NoError(t, err)
	pullOpts.Reference = opts.Reference
	_, err = Pull(ctx, pullOpts)
	require.NoError(t, err)

	removed, err := Prune(cacheDir, second.LayerDigest)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)

	_, err = os.Stat(filepath.Join(cacheDir, "sha256-"+layerDigestHex(first.LayerDigest)))
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(cacheDir, "sha256-"+layerDigestHex(second.LayerDigest), rootfsEntry))
	require.NoError(t, err)
}

func TestValidation(t *testing.T) {
	ctx := context.Background()
	_, err := Pull(ctx, Options{})
	require.Error(t, err)

	_, err = Push(ctx, filepath.Join(t.TempDir(), "missing"), Options{Reference: "example.com/x:y"})
	require.Error(t, err)

	empty := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.WriteFile(empty, nil, 0o644))
	_, err = Push(ctx, empty, Options{Reference: "example.com/x:y"})
	require.Error(t, err)

	_, err = parseReference("", false)
	require.Error(t, err)
}

func TestPruneMissingCacheDir(t *testing.T) {
	removed, err := Prune(filepath.Join(t.TempDir(), "does-not-exist"), "")
	require.NoError(t, err)
	assert.Zero(t, removed)
}
