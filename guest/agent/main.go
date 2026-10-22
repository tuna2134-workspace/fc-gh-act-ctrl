//go:build linux

// Command fcghact-guest runs inside the microVM. It reads the job metadata from
// Firecracker's MMDS, writes /etc/resolv.conf, starts the ephemeral Actions
// runner with the JIT configuration, and powers the VM off when the runner
// exits.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	defaultMMDSBase    = "http://169.254.169.254"
	defaultRunnerDir   = "/opt/actions-runner"
	defaultWaitTimeout = 3 * time.Minute
)

// metadata mirrors vm.Metadata on the host side.
type metadata struct {
	JobID      int64     `json:"job_id"`
	Repository string    `json:"repository"`
	RunnerName string    `json:"runner_name"`
	WorkFolder string    `json:"work_folder"`
	JITConfig  string    `json:"jit_config"`
	DNS        []string  `json:"dns,omitempty"`
	IssuedAt   time.Time `json:"issued_at"`
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("fcghact-guest: ")

	// Always power off, even when the runner fails: the host reaps the VM based
	// on the Firecracker process exiting. os.Exit does not run defers, so the
	// exit code is stored before shutting down.
	code := 0
	if err := run(loadEnv()); err != nil {
		log.Printf("fatal: %v", err)
		code = 1
	}
	powerOff()
	os.Exit(code)
}

type config struct {
	mmdsBase    string
	runnerDir   string
	waitTimeout time.Duration
}

func loadEnv() config {
	cfg := config{
		mmdsBase:    envOr("FCGHACT_MMDS_BASE", defaultMMDSBase),
		runnerDir:   envOr("FCGHACT_RUNNER_DIR", defaultRunnerDir),
		waitTimeout: defaultWaitTimeout,
	}
	if v := os.Getenv("FCGHACT_MMDS_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.waitTimeout = d
		}
	}
	return cfg
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func run(cfg config) error {
	growRootFilesystem()

	ctx, cancel := context.WithTimeout(context.Background(), cfg.waitTimeout)
	defer cancel()

	client := newMMDSClient(cfg.mmdsBase)
	meta, err := client.waitForMetadata(ctx)
	if err != nil {
		return fmt.Errorf("read metadata from MMDS: %w", err)
	}
	if meta.JITConfig == "" {
		return errors.New("metadata has an empty jit_config")
	}
	if meta.RunnerName != "" {
		if err := syscall.Sethostname([]byte(meta.RunnerName)); err != nil {
			log.Printf("set hostname: %v", err)
		}
		if err := ensureHostsEntry("/etc/hosts", meta.RunnerName); err != nil {
			log.Printf("update /etc/hosts: %v", err)
		}
	}
	if err := writeResolvConf(meta.DNS); err != nil {
		log.Printf("write resolv.conf: %v", err)
	}
	log.Printf("starting runner %q for job %d (%s)", meta.RunnerName, meta.JobID, meta.Repository)
	code, err := runRunner(cfg.runnerDir, meta)
	if err != nil {
		return err
	}
	log.Printf("runner exited with code %d", code)
	return nil
}

// growRootFilesystem expands the ext4 root filesystem to the size of the
// virtio-blk device. The base image can therefore be grown later and new VMs
// will use the additional space; running VMs cannot be resized.
func growRootFilesystem() {
	if _, err := os.Stat("/dev/vda"); err != nil {
		return
	}
	bin, err := exec.LookPath("resize2fs")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "/dev/vda").CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		log.Printf("resize2fs /dev/vda: %v: %s", err, text)
		return
	}
	if strings.Contains(text, "Nothing to do") {
		return
	}
	log.Printf("root filesystem resized: %s", text)
}

// runRunner executes the Actions runner in ephemeral JIT mode.
func runRunner(dir string, meta *metadata) (int, error) {
	script := filepath.Join(dir, "run.sh")
	if _, err := os.Stat(script); err != nil {
		return 0, fmt.Errorf("runner script: %w", err)
	}
	// run.sh only accepts --jitconfig and --startuptype; --disableupdate is a
	// config.sh flag and is rejected by the run command.
	args := []string{"--jitconfig", meta.JITConfig}
	cmd := exec.Command(script, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"RUNNER_ALLOW_RUNASROOT=1",
		"HOME=/root",
	)
	if meta.WorkFolder != "" {
		if err := os.MkdirAll(meta.WorkFolder, 0o755); err != nil {
			log.Printf("create work folder %s: %v", meta.WorkFolder, err)
		}
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 1, err
}

// ensureHostsEntry adds 127.0.1.1 <hostname> so sudo and other tools can
// resolve the runner hostname without waiting on DNS.
func ensureHostsEntry(path, hostname string) error {
	if hostname == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		for _, field := range strings.Fields(line) {
			if field == hostname {
				return nil
			}
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "127.0.1.1 %s\n", hostname)
	return err
}

func writeResolvConf(dns []string) error {
	return writeResolvConfTo("/etc/resolv.conf", dns)
}

func writeResolvConfTo(path string, dns []string) error {
	if len(dns) == 0 {
		return nil
	}
	var b strings.Builder
	for _, server := range dns {
		if net.ParseIP(server) == nil {
			continue
		}
		b.WriteString("nameserver ")
		b.WriteString(server)
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return nil
	}
	tmp := path + ".fcghact.tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func powerOff() {
	_ = exec.Command("sync").Run()
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err == nil {
		return
	}
	// Fall back to userspace shutdown. The host kills the VMM on timeout if
	// this also fails.
	_ = exec.Command("poweroff", "-f").Run()
}

// mmdsClient speaks the Firecracker MMDS v2 token protocol.
type mmdsClient struct {
	base string
	http *http.Client
}

func newMMDSClient(base string) *mmdsClient {
	return &mmdsClient{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 5 * time.Second},
	}
}

func (c *mmdsClient) token(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/latest/api/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-metadata-token-ttl-seconds", "21600")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %s", resp.Status)
	}
	return strings.TrimSpace(string(body)), nil
}

func (c *mmdsClient) metadata(ctx context.Context, token string) (*metadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/latest/meta-data/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-metadata-token", token)
	// Firecracker defaults to IMDS formatting unless JSON is requested.
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata endpoint returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read metadata: %w", err)
	}
	var meta metadata
	if err := json.Unmarshal(body, &meta); err != nil {
		snippet := body
		if len(snippet) > 160 {
			snippet = snippet[:160]
		}
		return nil, fmt.Errorf("decode metadata (body starts with %q): %w", snippet, err)
	}
	return &meta, nil
}

func (c *mmdsClient) waitForMetadata(ctx context.Context) (*metadata, error) {
	backoff := 200 * time.Millisecond
	var lastErr error
	var lastLog time.Time
	for {
		token, err := c.token(ctx)
		if err == nil {
			meta, err := c.metadata(ctx, token)
			if err == nil {
				return meta, nil
			}
			lastErr = err
		} else {
			lastErr = err
		}
		// Always surface the first failure; repeating it every 30s keeps the
		// console useful without flooding it.
		if lastLog.IsZero() || time.Since(lastLog) >= 30*time.Second {
			log.Printf("waiting for MMDS metadata: %v", lastErr)
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(backoff):
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}
