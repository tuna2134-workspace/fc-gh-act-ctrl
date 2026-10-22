package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloneFileCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ext4")
	content := []byte("rootfs-content")
	require.NoError(t, os.WriteFile(src, content, 0o644))

	dst := filepath.Join(dir, "dst.ext4")
	reflinked, err := cloneFile(src, dst, false)
	require.NoError(t, err)
	assert.False(t, reflinked)

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, content, got)
}

func TestCloneFileReflinkOrFallback(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ext4")
	content := make([]byte, 1<<16)
	for i := range content {
		content[i] = byte(i)
	}
	require.NoError(t, os.WriteFile(src, content, 0o644))

	dst := filepath.Join(dir, "dst.ext4")
	_, err := cloneFile(src, dst, true)
	require.NoError(t, err)

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, content, got)
}

func TestCloneFileSparse(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ext4")
	f, err := os.Create(src)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(1<<20))
	_, err = f.WriteAt([]byte("start"), 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("middle"), 512<<10)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("end"), (1<<20)-3)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	dst := filepath.Join(dir, "dst.ext4")
	_, err = cloneFile(src, dst, false)
	require.NoError(t, err)

	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, int64(1<<20), info.Size())

	want, err := os.ReadFile(src)
	require.NoError(t, err)
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestCloneFileFailsWhenDestinationExists(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	require.NoError(t, os.WriteFile(src, []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(dst, []byte("b"), 0o644))

	_, err := cloneFile(src, dst, false)
	require.Error(t, err)
}

func TestGuestMAC(t *testing.T) {
	mac := guestMAC("0f8fad5b-d9cb-469f-a165-70867728950e")
	assert.Equal(t, "02:00:0f:8f:ad:5b", mac)
	assert.Equal(t, mac, guestMAC("0f8fad5b-d9cb-469f-a165-70867728950e"))
	assert.Equal(t, "02:00:00:00:00:01", guestMAC("not-a-uuid"))
}

func TestStateFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := &stateFile{ID: "x", JobID: 5, Tap: "fctap1", Status: "running"}
	require.NoError(t, writeState(dir, st))
	got, err := readState(dir)
	require.NoError(t, err)
	assert.Equal(t, st.Tap, got.Tap)
	assert.Equal(t, st.JobID, got.JobID)
}

func TestMetadataEnvelopeMatchesGuestPath(t *testing.T) {
	env := newMetadataEnvelope(Metadata{
		JobID:      42,
		Repository: "owner/repo",
		RunnerName: "runner",
		JITConfig:  "encoded",
	})
	raw, err := json.Marshal(env)
	require.NoError(t, err)

	// The guest reads /latest/meta-data/, i.e. latest->meta-data.
	var parsed struct {
		Latest struct {
			MetaData Metadata `json:"meta-data"`
		} `json:"latest"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))
	assert.Equal(t, int64(42), parsed.Latest.MetaData.JobID)
	assert.Equal(t, "owner/repo", parsed.Latest.MetaData.Repository)
	assert.Equal(t, "encoded", parsed.Latest.MetaData.JITConfig)
}

func TestSpecValidation(t *testing.T) {
	valid := Spec{JobID: 1, Repository: "o/r", RunnerName: "r", JITConfig: "j"}
	require.NoError(t, valid.validate())
	bad := valid
	bad.JITConfig = ""
	require.Error(t, bad.validate())
	bad = valid
	bad.ID = "../escape"
	require.Error(t, bad.validate())
}
