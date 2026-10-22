package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/config"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/network"
)

// Options configures the Firecracker backend.
type Options struct {
	Config      config.FirecrackerConfig
	Network     *network.Manager
	Logger      *slog.Logger
	BootTimeout time.Duration
	// SkipValidation is used by cleanup, which must work even when the kernel
	// or rootfs files have been moved away.
	SkipValidation bool
}

// FirecrackerBackend implements Backend with one Firecracker process per VM.
type FirecrackerBackend struct {
	cfg         config.FirecrackerConfig
	net         *network.Manager
	log         *slog.Logger
	fcLogger    *logrus.Logger
	bootTimeout time.Duration

	mu     sync.Mutex
	active map[string]*machineHandle
	closed bool
}

var _ Backend = (*FirecrackerBackend)(nil)

// NewFirecracker validates the runtime files and returns a backend.
func NewFirecracker(opts Options) (*FirecrackerBackend, error) {
	if opts.Network == nil {
		return nil, errors.New("vm: network manager is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.BootTimeout <= 0 {
		opts.BootTimeout = 3 * time.Minute
	}
	if opts.Config.Binary == "" {
		opts.Config.Binary = "firecracker"
	}
	if !opts.SkipValidation {
		if _, err := exec.LookPath(opts.Config.Binary); err != nil {
			return nil, fmt.Errorf("firecracker binary %q not found: %w", opts.Config.Binary, err)
		}
		for _, path := range []string{opts.Config.KernelImage, opts.Config.RootfsImage} {
			if _, err := os.Stat(path); err != nil {
				return nil, fmt.Errorf("required image %s: %w", path, err)
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(opts.Config.StateDir, "vms"), 0o750); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	fcLogger := logrus.New()
	fcLogger.SetOutput(os.Stderr)
	fcLogger.SetLevel(logrus.WarnLevel)
	fcLogger.SetFormatter(&logrus.TextFormatter{DisableTimestamp: true, DisableColors: true})

	return &FirecrackerBackend{
		cfg:         opts.Config,
		net:         opts.Network,
		log:         opts.Logger,
		fcLogger:    fcLogger,
		bootTimeout: opts.BootTimeout,
		active:      make(map[string]*machineHandle),
	}, nil
}

func (b *FirecrackerBackend) vmsDir() string { return filepath.Join(b.cfg.StateDir, "vms") }
func (b *FirecrackerBackend) vmDir(id string) string {
	return filepath.Join(b.vmsDir(), id)
}

// Start boots a microVM and waits until Firecracker reports it is running.
func (b *FirecrackerBackend) Start(ctx context.Context, spec Spec) (Handle, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	if spec.ID == "" {
		spec.ID = uuid.NewString()
	} else if _, err := uuid.Parse(spec.ID); err != nil {
		return nil, fmt.Errorf("vm: id must be a UUID: %w", err)
	}
	if spec.WorkFolder == "" {
		spec.WorkFolder = "/opt/actions-runner/_work"
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("vm: backend is shut down")
	}
	b.mu.Unlock()

	dir := b.vmDir(spec.ID)
	socketPath := filepath.Join(dir, "firecracker.sock")
	if len(socketPath) >= 100 {
		return nil, fmt.Errorf("vm: state directory path is too long for a unix socket (%d bytes): %s", len(socketPath), socketPath)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create vm dir: %w", err)
	}

	lease, err := b.net.Acquire()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("allocate network: %w", err)
	}
	cleanup := func() {
		if lease != nil {
			if err := b.net.Release(lease); err != nil {
				b.log.Warn("release network lease", "vm", spec.ID, "error", err)
			}
		}
		_ = os.RemoveAll(dir)
	}

	diskPath := filepath.Join(dir, "rootfs.ext4")
	reflinked, err := cloneFile(b.cfg.RootfsImage, diskPath, b.cfg.Reflink)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("prepare rootfs: %w", err)
	}

	consolePath := filepath.Join(dir, "console.log")
	console, err := os.OpenFile(consolePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("open console log: %w", err)
	}
	fail := func(err error) (Handle, error) {
		_ = console.Close()
		cleanup()
		return nil, err
	}

	state := &stateFile{
		ID:          spec.ID,
		JobID:       spec.JobID,
		Repository:  spec.Repository,
		RunnerName:  spec.RunnerName,
		IP:          lease.IP.String(),
		Tap:         lease.Tap,
		SocketPath:  socketPath,
		ConsolePath: consolePath,
		DiskPath:    diskPath,
		Status:      "starting",
		CreatedAt:   time.Now().UTC(),
	}
	if err := writeState(dir, state); err != nil {
		b.log.Warn("write vm state", "vm", spec.ID, "error", err)
	}

	mac := guestMAC(spec.ID)
	vmCtx, vmCancel := context.WithCancel(context.Background())

	cmd := exec.Command(b.cfg.Binary, "--id", spec.ID, "--api-sock", socketPath)
	cmd.Stdout = console
	cmd.Stderr = console

	fcCfg := firecracker.Config{
		SocketPath:      socketPath,
		KernelImagePath: b.cfg.KernelImage,
		KernelArgs:      b.cfg.BootArgs,
		Drives: []models.Drive{{
			DriveID:      firecracker.String("rootfs"),
			PathOnHost:   firecracker.String(diskPath),
			IsRootDevice: firecracker.Bool(true),
			IsReadOnly:   firecracker.Bool(false),
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(b.cfg.VCPUCount),
			MemSizeMib: firecracker.Int64(b.cfg.MemSizeMiB),
			Smt:        firecracker.Bool(false),
		},
		NetworkInterfaces: firecracker.NetworkInterfaces{{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				MacAddress:  mac,
				HostDevName: lease.Tap,
				IPConfiguration: &firecracker.IPConfiguration{
					IPAddr:      net.IPNet{IP: lease.IP, Mask: lease.Mask},
					Gateway:     lease.Gateway,
					Nameservers: lease.DNS,
					IfName:      "eth0",
				},
			},
			AllowMMDS: true,
		}},
		MmdsVersion: firecracker.MMDSv2,
		LogPath:     filepath.Join(dir, "firecracker.log"),
		LogLevel:    b.cfg.LogLevel,
		MetricsPath: filepath.Join(dir, "firecracker.metrics"),
		// Do not forward controller signals to every VMM: shutdown is managed
		// explicitly through Stop.
		ForwardSignals: []os.Signal{},
	}

	entry := b.fcLogger.WithField("vm", spec.ID)
	machine, err := firecracker.NewMachine(vmCtx, fcCfg,
		firecracker.WithLogger(entry),
		firecracker.WithProcessRunner(cmd),
	)
	if err != nil {
		vmCancel()
		return fail(fmt.Errorf("create firecracker machine: %w", err))
	}

	meta := Metadata{
		JobID:      spec.JobID,
		Repository: spec.Repository,
		RunnerName: spec.RunnerName,
		WorkFolder: spec.WorkFolder,
		JITConfig:  spec.JITConfig,
		DNS:        lease.DNS,
		IssuedAt:   time.Now().UTC(),
	}
	machine.Handlers.FcInit = machine.Handlers.FcInit.Append(firecracker.NewSetMetadataHandler(meta))

	startCh := make(chan error, 1)
	go func() { startCh <- machine.Start(vmCtx) }()

	timer := time.NewTimer(b.bootTimeout)
	defer timer.Stop()
	select {
	case err := <-startCh:
		if err != nil {
			vmCancel()
			_ = machine.StopVMM()
			_ = killVMsBySocket(context.Background(), socketPath)
			return fail(fmt.Errorf("boot microvm: %w", err))
		}
	case <-timer.C:
		vmCancel()
		_ = machine.StopVMM()
		_ = killVMsBySocket(context.Background(), socketPath)
		return fail(fmt.Errorf("microvm did not boot within %s", b.bootTimeout))
	case <-ctx.Done():
		vmCancel()
		_ = machine.StopVMM()
		_ = killVMsBySocket(context.Background(), socketPath)
		return fail(ctx.Err())
	}

	h := &machineHandle{
		id:        spec.ID,
		ip:        lease.IP.String(),
		runner:    spec.RunnerName,
		startedAt: time.Now().UTC(),
		socket:    socketPath,
		dir:       dir,
		lease:     lease,
		console:   console,
		machine:   machine,
		cancel:    vmCancel,
		done:      make(chan struct{}),
		log:       b.log.With("vm", spec.ID, "job_id", spec.JobID, "runner", spec.RunnerName),
		backend:   b,
	}

	state.Status = "running"
	state.StartedAt = h.startedAt
	if err := writeState(dir, state); err != nil {
		b.log.Warn("update vm state", "vm", spec.ID, "error", err)
	}

	b.mu.Lock()
	b.active[spec.ID] = h
	b.mu.Unlock()

	b.log.Info("microvm started",
		"vm", spec.ID,
		"job_id", spec.JobID,
		"repository", spec.Repository,
		"runner", spec.RunnerName,
		"ip", lease.IP.String(),
		"tap", lease.Tap,
		"reflink", reflinked,
	)

	go func() { h.finish(machine.Wait(context.Background())) }()
	return h, nil
}

// archiveLogs moves the VM logs into the persistent log directory.
func (b *FirecrackerBackend) archiveLogs(dir, id string) {
	target := filepath.Join(b.cfg.StateDir, "logs", id)
	if err := os.MkdirAll(target, 0o750); err != nil {
		b.log.Warn("create log archive", "vm", id, "error", err)
		return
	}
	for _, name := range []string{"console.log", "firecracker.log", "vm.json"} {
		src := filepath.Join(dir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, filepath.Join(target, name)); err != nil {
			b.log.Warn("archive log", "vm", id, "file", name, "error", err)
		}
	}
}

func (b *FirecrackerBackend) unregister(id string) {
	b.mu.Lock()
	delete(b.active, id)
	b.mu.Unlock()
}

// Reap kills leftover VMMs and removes their disks and TAP devices.
func (b *FirecrackerBackend) Reap(ctx context.Context) error {
	entries, err := os.ReadDir(b.vmsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(b.vmsDir(), entry.Name())
		var state *stateFile
		if st, err := readState(dir); err == nil {
			state = st
		}
		socket := filepath.Join(dir, "firecracker.sock")
		tap := ""
		if state != nil {
			if state.SocketPath != "" {
				socket = state.SocketPath
			}
			tap = state.Tap
		}
		if pids := findProcessesBySocket(socket); len(pids) > 0 {
			b.log.Warn("reaping orphaned firecracker process", "vm", entry.Name(), "pids", pids)
			if err := stopProcesses(10*time.Second, pids...); err != nil {
				errs = append(errs, fmt.Errorf("stop orphan %s: %w", entry.Name(), err))
			}
		}
		if tap != "" {
			if err := network.ReleaseByName(tap); err != nil {
				errs = append(errs, fmt.Errorf("release tap %s: %w", tap, err))
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", dir, err))
		}
	}
	if err := b.net.ReapTaps(); err != nil {
		errs = append(errs, fmt.Errorf("reap taps: %w", err))
	}
	select {
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	default:
	}
	return errors.Join(errs...)
}

// Shutdown stops every VM owned by this backend.
func (b *FirecrackerBackend) Shutdown(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	handles := make([]*machineHandle, 0, len(b.active))
	for _, h := range b.active {
		handles = append(handles, h)
	}
	b.mu.Unlock()

	var g errgroup.Group
	for _, h := range handles {
		h := h
		g.Go(func() error { return h.Stop(ctx) })
	}
	return g.Wait()
}

type machineHandle struct {
	id        string
	ip        string
	runner    string
	startedAt time.Time
	socket    string
	dir       string
	lease     *network.Lease
	console   *os.File
	machine   *firecracker.Machine
	cancel    context.CancelFunc
	backend   *FirecrackerBackend
	log       *slog.Logger

	done chan struct{}

	errMu sync.Mutex
	err   error

	stopOnce    sync.Once
	cleanupOnce sync.Once
}

var _ Handle = (*machineHandle)(nil)

func (h *machineHandle) ID() string           { return h.id }
func (h *machineHandle) IP() string           { return h.ip }
func (h *machineHandle) RunnerName() string   { return h.runner }
func (h *machineHandle) StartedAt() time.Time { return h.startedAt }
func (h *machineHandle) ConsolePath() string  { return filepath.Join(h.dir, "console.log") }
func (h *machineHandle) Wait() error {
	<-h.done
	h.errMu.Lock()
	defer h.errMu.Unlock()
	return h.err
}

func (h *machineHandle) Stop(ctx context.Context) error {
	h.stopOnce.Do(func() {
		if err := h.machine.StopVMM(); err != nil {
			h.log.Debug("stop vmm", "error", err)
		}
		select {
		case <-h.done:
			return
		case <-time.After(10 * time.Second):
		}
		h.log.Warn("firecracker did not exit after SIGTERM, killing")
		if pid, err := h.machine.PID(); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if err := killVMsBySocket(context.Background(), h.socket); err != nil {
			h.log.Warn("kill firecracker", "error", err)
		}
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
		}
	})
	select {
	case <-h.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *machineHandle) finish(err error) {
	h.errMu.Lock()
	h.err = err
	h.errMu.Unlock()

	h.cleanupOnce.Do(func() {
		h.cancel()
		if h.console != nil {
			_ = h.console.Close()
		}
		if h.backend != nil {
			if err := h.backend.net.Release(h.lease); err != nil {
				h.log.Warn("release network lease", "error", err)
			}
			h.backend.unregister(h.id)
		}
		if h.backend != nil && h.backend.cfg.KeepLogs {
			h.backend.archiveLogs(h.dir, h.id)
		}
		if err := os.RemoveAll(h.dir); err != nil {
			h.log.Warn("remove vm dir", "dir", h.dir, "error", err)
		}
		if err != nil {
			h.log.Info("microvm exited", "error", err)
		} else {
			h.log.Info("microvm exited")
		}
	})
	close(h.done)
}

// guestMAC derives a stable, locally administered unicast MAC from the VM UUID.
func guestMAC(id string) string {
	u, err := uuid.Parse(id)
	if err != nil {
		return "02:00:00:00:00:01"
	}
	return net.HardwareAddr{0x02, 0x00, u[0], u[1], u[2], u[3]}.String()
}
