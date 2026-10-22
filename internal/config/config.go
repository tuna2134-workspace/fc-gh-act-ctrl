// Package config loads and validates the controller configuration.
//
// Precedence: built-in defaults < YAML file < environment variables.
// Environment variables use the FCGHACT_ prefix (for example
// FCGHACT_SERVER_LISTEN).
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that can be decoded from YAML and environment
// variables using time.ParseDuration syntax (for example "30s", "1h30m").
type Duration time.Duration

// Duration returns the standard library representation.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d *Duration) set(text string) error {
	v, err := time.ParseDuration(strings.TrimSpace(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	*d = Duration(v)
	return nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	return d.set(s)
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// UnmarshalText implements encoding.TextUnmarshaler (used by caarlos0/env).
func (d *Duration) UnmarshalText(text []byte) error { return d.set(string(text)) }

// Config is the root configuration.
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Log         LogConfig         `yaml:"log"`
	GitHub      GitHubConfig      `yaml:"github"`
	Runner      RunnerConfig      `yaml:"runner"`
	Firecracker FirecrackerConfig `yaml:"firecracker"`
	Network     NetworkConfig     `yaml:"network"`
}

// ServerConfig controls the HTTP server.
type ServerConfig struct {
	Listen          string   `yaml:"listen" env:"SERVER_LISTEN"`
	WebhookPath     string   `yaml:"webhook_path" env:"SERVER_WEBHOOK_PATH"`
	HealthPath      string   `yaml:"health_path" env:"SERVER_HEALTH_PATH"`
	ReadyPath       string   `yaml:"ready_path" env:"SERVER_READY_PATH"`
	MetricsPath     string   `yaml:"metrics_path" env:"SERVER_METRICS_PATH"`
	ReadTimeout     Duration `yaml:"read_timeout" env:"SERVER_READ_TIMEOUT"`
	WriteTimeout    Duration `yaml:"write_timeout" env:"SERVER_WRITE_TIMEOUT"`
	ShutdownTimeout Duration `yaml:"shutdown_timeout" env:"SERVER_SHUTDOWN_TIMEOUT"`
}

// LogConfig controls structured logging.
type LogConfig struct {
	Level  string `yaml:"level" env:"LOG_LEVEL"`   // debug, info, warn, error
	Format string `yaml:"format" env:"LOG_FORMAT"` // json, text
}

// GitHubConfig describes the GitHub App used by the controller.
type GitHubConfig struct {
	AppID             int64    `yaml:"app_id" env:"GITHUB_APP_ID"`
	PrivateKeyPath    string   `yaml:"private_key_path" env:"GITHUB_PRIVATE_KEY_PATH"`
	PrivateKey        string   `yaml:"private_key" env:"GITHUB_PRIVATE_KEY"`
	WebhookSecret     string   `yaml:"webhook_secret" env:"GITHUB_WEBHOOK_SECRET"`
	WebhookSecretFile string   `yaml:"webhook_secret_file" env:"GITHUB_WEBHOOK_SECRET_FILE"`
	BaseURL           string   `yaml:"base_url" env:"GITHUB_BASE_URL"`
	InstallationID    int64    `yaml:"installation_id" env:"GITHUB_INSTALLATION_ID"`
	Scope             string   `yaml:"scope" env:"GITHUB_SCOPE"` // repo or org
	Repositories      []string `yaml:"repositories" env:"GITHUB_REPOSITORIES" envSeparator:","`
	Organizations     []string `yaml:"organizations" env:"GITHUB_ORGANIZATIONS" envSeparator:","`
}

// RunnerConfig describes the runner and scheduling behaviour.
type RunnerConfig struct {
	Labels              []string `yaml:"labels" env:"RUNNER_LABELS" envSeparator:","`
	RequireLabels       []string `yaml:"require_labels" env:"RUNNER_REQUIRE_LABELS" envSeparator:","`
	RunnerGroupID       int64    `yaml:"runner_group_id" env:"RUNNER_GROUP_ID"`
	WorkFolder          string   `yaml:"work_folder" env:"RUNNER_WORK_FOLDER"`
	NamePrefix          string   `yaml:"name_prefix" env:"RUNNER_NAME_PREFIX"`
	MaxConcurrentJobs   int      `yaml:"max_concurrent_jobs" env:"RUNNER_MAX_CONCURRENT_JOBS"`
	MaxJobsPerRepo      int      `yaml:"max_jobs_per_repo" env:"RUNNER_MAX_JOBS_PER_REPO"`
	QueueSize           int      `yaml:"queue_size" env:"RUNNER_QUEUE_SIZE"`
	BootTimeout         Duration `yaml:"boot_timeout" env:"RUNNER_BOOT_TIMEOUT"`
	RegistrationTimeout Duration `yaml:"registration_timeout" env:"RUNNER_REGISTRATION_TIMEOUT"`
	JobTimeout          Duration `yaml:"job_timeout" env:"RUNNER_JOB_TIMEOUT"`
	MonitorInterval     Duration `yaml:"monitor_interval" env:"RUNNER_MONITOR_INTERVAL"`
}

// FirecrackerConfig describes the microVM runtime.
type FirecrackerConfig struct {
	Binary      string `yaml:"binary" env:"FIRECRACKER_BINARY"`
	KernelImage string `yaml:"kernel_image" env:"FIRECRACKER_KERNEL_IMAGE"`
	RootfsImage string `yaml:"rootfs_image" env:"FIRECRACKER_ROOTFS_IMAGE"`
	StateDir    string `yaml:"state_dir" env:"FIRECRACKER_STATE_DIR"`
	VCPUCount   int64  `yaml:"vcpu_count" env:"FIRECRACKER_VCPU_COUNT"`
	MemSizeMiB  int64  `yaml:"mem_size_mib" env:"FIRECRACKER_MEM_SIZE_MIB"`
	BootArgs    string `yaml:"boot_args" env:"FIRECRACKER_BOOT_ARGS"`
	LogLevel    string `yaml:"log_level" env:"FIRECRACKER_LOG_LEVEL"`
	// Reflink enables copy-on-write rootfs clones (FICLONE). Falls back to a
	// full copy when the filesystem does not support it.
	Reflink bool `yaml:"reflink" env:"FIRECRACKER_REFLINK"`
}

// NetworkConfig describes the host networking used by microVMs.
type NetworkConfig struct {
	ManageBridge bool     `yaml:"manage_bridge" env:"NETWORK_MANAGE_BRIDGE"`
	Bridge       string   `yaml:"bridge" env:"NETWORK_BRIDGE"`
	CIDR         string   `yaml:"cidr" env:"NETWORK_CIDR"`
	Gateway      string   `yaml:"gateway" env:"NETWORK_GATEWAY"`
	DNS          []string `yaml:"dns" env:"NETWORK_DNS" envSeparator:","`
	MTU          int      `yaml:"mtu" env:"NETWORK_MTU"`
	TapPrefix    string   `yaml:"tap_prefix" env:"NETWORK_TAP_PREFIX"`
}

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:          ":8080",
			WebhookPath:     "/webhook",
			HealthPath:      "/healthz",
			ReadyPath:       "/readyz",
			MetricsPath:     "/metrics",
			ReadTimeout:     Duration(30 * time.Second),
			WriteTimeout:    Duration(30 * time.Second),
			ShutdownTimeout: Duration(30 * time.Second),
		},
		Log: LogConfig{Level: "info", Format: "json"},
		GitHub: GitHubConfig{
			BaseURL: "https://api.github.com",
			Scope:   "repo",
		},
		Runner: RunnerConfig{
			Labels:              []string{"self-hosted", "linux", "x64", "fcghact"},
			RequireLabels:       []string{"fcghact"},
			RunnerGroupID:       1,
			WorkFolder:          "/opt/actions-runner/_work",
			NamePrefix:          "fcghact",
			MaxConcurrentJobs:   4,
			MaxJobsPerRepo:      2,
			QueueSize:           128,
			BootTimeout:         Duration(3 * time.Minute),
			RegistrationTimeout: Duration(5 * time.Minute),
			JobTimeout:          Duration(60 * time.Minute),
			MonitorInterval:     Duration(30 * time.Second),
		},
		Firecracker: FirecrackerConfig{
			Binary:      "firecracker",
			KernelImage: "/var/lib/fcghact/vmlinux",
			RootfsImage: "/var/lib/fcghact/rootfs.ext4",
			StateDir:    "/var/lib/fcghact",
			VCPUCount:   2,
			MemSizeMiB:  2048,
			BootArgs:    "console=ttyS0 reboot=k panic=1 pci=off net.ifnames=0 root=/dev/vda rw rootfstype=ext4",
			LogLevel:    "Warning",
			Reflink:     true,
		},
		Network: NetworkConfig{
			ManageBridge: true,
			Bridge:       "fcghact0",
			CIDR:         "172.31.0.0/16",
			Gateway:      "172.31.0.1",
			DNS:          []string{"1.1.1.1", "8.8.8.8"},
			MTU:          1500,
			TapPrefix:    "fctap",
		},
	}
}

// Load reads the configuration from path (optional), applies environment
// overrides, then validates the result.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	if err := env.ParseWithOptions(cfg, env.Options{Prefix: "FCGHACT_"}); err != nil {
		return nil, fmt.Errorf("parse environment: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks cross-field invariants.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Server.Listen == "" {
		add("server.listen must not be empty")
	}
	for _, p := range []string{c.Server.WebhookPath, c.Server.HealthPath, c.Server.ReadyPath, c.Server.MetricsPath} {
		if !strings.HasPrefix(p, "/") {
			add("server paths must start with '/': %q", p)
		}
	}
	if c.Server.ReadTimeout <= 0 || c.Server.WriteTimeout <= 0 || c.Server.ShutdownTimeout <= 0 {
		add("server timeouts must be positive")
	}

	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "warning", "error":
	default:
		add("log.level must be one of debug, info, warn, error: %q", c.Log.Level)
	}
	switch strings.ToLower(c.Log.Format) {
	case "json", "text":
	default:
		add("log.format must be json or text: %q", c.Log.Format)
	}

	if c.GitHub.AppID <= 0 {
		add("github.app_id is required")
	}
	if c.GitHub.PrivateKeyPath == "" && c.GitHub.PrivateKey == "" {
		add("github.private_key_path or github.private_key is required")
	}
	if c.GitHub.WebhookSecret == "" && c.GitHub.WebhookSecretFile == "" {
		add("github.webhook_secret or github.webhook_secret_file is required")
	}
	switch strings.ToLower(c.GitHub.Scope) {
	case "repo", "org":
	default:
		add("github.scope must be repo or org: %q", c.GitHub.Scope)
	}
	for _, repo := range c.GitHub.Repositories {
		if !strings.Contains(repo, "/") {
			add("github.repositories entries must be owner/name: %q", repo)
		}
	}

	if len(c.Runner.Labels) == 0 {
		add("runner.labels must not be empty")
	}
	labelSet := make(map[string]struct{}, len(c.Runner.Labels))
	for _, l := range c.Runner.Labels {
		labelSet[strings.ToLower(l)] = struct{}{}
	}
	for _, l := range c.Runner.RequireLabels {
		if _, ok := labelSet[strings.ToLower(l)]; !ok {
			add("runner.require_labels entry %q is not part of runner.labels", l)
		}
	}
	if c.Runner.RunnerGroupID <= 0 {
		add("runner.runner_group_id must be positive")
	}
	if c.Runner.MaxConcurrentJobs <= 0 {
		add("runner.max_concurrent_jobs must be positive")
	}
	if c.Runner.MaxJobsPerRepo <= 0 {
		add("runner.max_jobs_per_repo must be positive")
	}
	if c.Runner.QueueSize <= 0 {
		add("runner.queue_size must be positive")
	}
	if c.Runner.BootTimeout <= 0 || c.Runner.RegistrationTimeout <= 0 || c.Runner.JobTimeout <= 0 {
		add("runner timeouts must be positive")
	}
	if c.Runner.MonitorInterval <= 0 {
		add("runner.monitor_interval must be positive")
	}

	if c.Firecracker.Binary == "" {
		add("firecracker.binary must not be empty")
	}
	if c.Firecracker.KernelImage == "" || c.Firecracker.RootfsImage == "" || c.Firecracker.StateDir == "" {
		add("firecracker kernel_image, rootfs_image and state_dir are required")
	}
	if c.Firecracker.VCPUCount <= 0 {
		add("firecracker.vcpu_count must be positive")
	}
	if c.Firecracker.MemSizeMiB <= 0 {
		add("firecracker.mem_size_mib must be positive")
	}
	if strings.Contains(c.Firecracker.BootArgs, "ip=") {
		add("firecracker.boot_args must not contain ip= (it is generated from network config)")
	}

	if _, _, err := net.ParseCIDR(c.Network.CIDR); err != nil {
		add("network.cidr is invalid: %v", err)
	}
	gw := net.ParseIP(c.Network.Gateway)
	if gw == nil || gw.To4() == nil {
		add("network.gateway must be a valid IPv4 address: %q", c.Network.Gateway)
	} else if _, network, err := net.ParseCIDR(c.Network.CIDR); err == nil && !network.Contains(gw) {
		add("network.gateway %s is outside network.cidr %s", c.Network.Gateway, c.Network.CIDR)
	}
	if c.Network.Bridge == "" {
		add("network.bridge must not be empty")
	}
	if c.Network.MTU < 576 {
		add("network.mtu must be at least 576")
	}
	if c.Network.TapPrefix == "" {
		add("network.tap_prefix must not be empty")
	}
	for _, d := range c.Network.DNS {
		if net.ParseIP(d) == nil {
			add("network.dns contains an invalid IP: %q", d)
		}
	}

	return errors.Join(errs...)
}

// WebhookSecretBytes resolves the webhook secret, preferring the file variant.
func (c *GitHubConfig) WebhookSecretBytes() ([]byte, error) {
	if c.WebhookSecretFile != "" {
		b, err := os.ReadFile(c.WebhookSecretFile)
		if err != nil {
			return nil, fmt.Errorf("read webhook secret file: %w", err)
		}
		return []byte(strings.TrimSpace(string(b))), nil
	}
	if c.WebhookSecret == "" {
		return nil, errors.New("webhook secret is empty")
	}
	return []byte(c.WebhookSecret), nil
}

// PrivateKeyBytes resolves the GitHub App private key PEM.
func (c *GitHubConfig) PrivateKeyBytes() ([]byte, error) {
	if c.PrivateKeyPath != "" {
		b, err := os.ReadFile(c.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read private key: %w", err)
		}
		return b, nil
	}
	if c.PrivateKey == "" {
		return nil, errors.New("private key is empty")
	}
	// Allow "\n" escapes when the key is supplied inline via env/Kubernetes.
	return []byte(strings.ReplaceAll(c.PrivateKey, `\n`, "\n")), nil
}
