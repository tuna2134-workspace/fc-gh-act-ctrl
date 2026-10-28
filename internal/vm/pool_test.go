package vm

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClonePoolFillsAndRefills(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	content := []byte("rootfs-content")
	require.NoError(t, os.WriteFile(base, content, 0o644))

	pool := newClonePool(filepath.Join(dir, "pool"), base, 1, false,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	pool.start()
	defer pool.close()

	var first string
	require.Eventually(t, func() bool {
		first = pool.take()
		return first != ""
	}, 5*time.Second, 10*time.Millisecond)
	got, err := os.ReadFile(first)
	require.NoError(t, err)
	assert.Equal(t, content, got)

	// Taking one clone wakes the filler again.
	var second string
	require.Eventually(t, func() bool {
		second = pool.take()
		return second != ""
	}, 5*time.Second, 10*time.Millisecond)
	assert.NotEqual(t, first, second)
}

func TestClonePoolDisabled(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.ext4")
	require.NoError(t, os.WriteFile(base, []byte("x"), 0o644))
	pool := newClonePool(filepath.Join(dir, "pool"), base, 0, false, nil)
	pool.start()
	assert.Empty(t, pool.take())
	pool.close()
}
