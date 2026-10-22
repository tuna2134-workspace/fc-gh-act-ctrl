package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("FCGHACT_GITHUB_APP_ID", "1234")
	t.Setenv("FCGHACT_GITHUB_PRIVATE_KEY_PATH", "/etc/fcghact/app.pem")
	t.Setenv("FCGHACT_GITHUB_WEBHOOK_SECRET", "super-secret")
}

func TestLoadDefaultsAndEnvOverrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("FCGHACT_SERVER_LISTEN", "127.0.0.1:9090")
	t.Setenv("FCGHACT_RUNNER_MAX_CONCURRENT_JOBS", "8")
	t.Setenv("FCGHACT_NETWORK_DNS", "9.9.9.9,149.112.112.112")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9090", cfg.Server.Listen)
	assert.Equal(t, 8, cfg.Runner.MaxConcurrentJobs)
	assert.Equal(t, []string{"9.9.9.9", "149.112.112.112"}, cfg.Network.DNS)
	assert.Equal(t, 2048, int(cfg.Firecracker.MemSizeMiB))
	assert.Equal(t, "repo", cfg.GitHub.Scope)
	assert.True(t, cfg.Runner.ResourceLabels)
}

func TestLoadYAMLOverlayAndDuration(t *testing.T) {
	setRequiredEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := `
server:
  listen: ":9999"
  shutdown_timeout: 45s
runner:
  labels: ["self-hosted", "fcghact", "gpu"]
  require_labels: ["fcghact"]
  max_concurrent_jobs: 2
  job_timeout: 90m
network:
  dns: ["1.1.1.1"]
`
	require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, ":9999", cfg.Server.Listen)
	assert.Equal(t, 45*time.Second, cfg.Server.ShutdownTimeout.Duration())
	assert.Equal(t, []string{"self-hosted", "fcghact", "gpu"}, cfg.Runner.Labels)
	assert.Equal(t, 2, cfg.Runner.MaxConcurrentJobs)
	assert.Equal(t, 90*time.Minute, cfg.Runner.JobTimeout.Duration())
	assert.Equal(t, []string{"1.1.1.1"}, cfg.Network.DNS)
	// Values absent from the file keep their defaults.
	assert.Equal(t, 30*time.Second, cfg.Runner.MonitorInterval.Duration())
	assert.Equal(t, "fctap", cfg.Network.TapPrefix)
}

func TestValidateRejectsBadValues(t *testing.T) {
	base := func() *Config {
		cfg := Default()
		cfg.GitHub.AppID = 1
		cfg.GitHub.PrivateKeyPath = "/tmp/key.pem"
		cfg.GitHub.WebhookSecret = "secret"
		return cfg
	}

	t.Run("require label not in labels", func(t *testing.T) {
		cfg := base()
		cfg.Runner.RequireLabels = []string{"gpu"}
		require.ErrorContains(t, cfg.Validate(), "require_labels")
	})
	t.Run("invalid cidr", func(t *testing.T) {
		cfg := base()
		cfg.Network.CIDR = "not-a-cidr"
		require.ErrorContains(t, cfg.Validate(), "network.cidr")
	})
	t.Run("gateway outside cidr", func(t *testing.T) {
		cfg := base()
		cfg.Network.Gateway = "10.0.0.1"
		require.ErrorContains(t, cfg.Validate(), "outside network.cidr")
	})
	t.Run("ip in boot args", func(t *testing.T) {
		cfg := base()
		cfg.Firecracker.BootArgs += " ip=1.2.3.4::1.2.3.1:255.255.255.0::eth0:off"
		require.ErrorContains(t, cfg.Validate(), "ip=")
	})
	t.Run("bad log level", func(t *testing.T) {
		cfg := base()
		cfg.Log.Level = "verbose"
		require.ErrorContains(t, cfg.Validate(), "log.level")
	})
}

func TestWebhookSecretAndPrivateKeyResolution(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")
	require.NoError(t, os.WriteFile(secretFile, []byte("from-file\n"), 0o600))
	keyFile := filepath.Join(dir, "app.pem")
	require.NoError(t, os.WriteFile(keyFile, []byte("PEM"), 0o600))

	gh := GitHubConfig{WebhookSecretFile: secretFile, PrivateKeyPath: keyFile}
	secret, err := gh.WebhookSecretBytes()
	require.NoError(t, err)
	assert.Equal(t, "from-file", string(secret))
	key, err := gh.PrivateKeyBytes()
	require.NoError(t, err)
	assert.Equal(t, "PEM", string(key))

	inline := GitHubConfig{PrivateKey: "line1\\nline2"}
	key, err = inline.PrivateKeyBytes()
	require.NoError(t, err)
	assert.Equal(t, "line1\nline2", string(key))
}

func TestLoadPartialSkipsValidation(t *testing.T) {
	cfg, err := LoadPartial("")
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/fcghact/rootfs.ext4", cfg.Firecracker.RootfsImage)
	assert.True(t, cfg.Rootfs.Prune)
	assert.Equal(t, "info", cfg.Log.Level)
}

func TestRootfsOCIConfig(t *testing.T) {
	t.Setenv("FCGHACT_ROOTFS_OCI_REFERENCE", "ghcr.io/owner/fcghact-rootfs:v1")
	t.Setenv("FCGHACT_ROOTFS_PRUNE", "false")
	t.Setenv("FCGHACT_ROOTFS_TOKEN_ENV", "GHCR_TOKEN")
	cfg, err := LoadPartial("")
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/owner/fcghact-rootfs:v1", cfg.Rootfs.OCIReference)
	assert.False(t, cfg.Rootfs.Prune)
	assert.Equal(t, "GHCR_TOKEN", cfg.Rootfs.TokenEnv)
}

func TestValidateAllowsOCIWithoutLocalRootfs(t *testing.T) {
	cfg := Default()
	cfg.GitHub.AppID = 1
	cfg.GitHub.PrivateKeyPath = "/tmp/key.pem"
	cfg.GitHub.WebhookSecret = "secret"
	cfg.Firecracker.RootfsImage = ""
	cfg.Rootfs.OCIReference = "ghcr.io/owner/fcghact-rootfs:v1"
	require.NoError(t, cfg.Validate())

	cfg.Rootfs.OCIReference = ""
	require.ErrorContains(t, cfg.Validate(), "rootfs_image")
}

func TestDurationText(t *testing.T) {
	var d Duration
	require.NoError(t, d.UnmarshalText([]byte("90s")))
	assert.Equal(t, 90*time.Second, d.Duration())
	require.Error(t, d.UnmarshalText([]byte("nope")))
}
