package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// findProcessesBySocket returns PIDs whose command line references one of the
// given socket paths. Firecracker is started with --api-sock <path>, which makes
// this a reliable way to identify an orphaned VMM after a controller crash.
func findProcessesBySocket(sockets ...string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil || len(raw) == 0 {
			continue
		}
		cmdline := strings.ReplaceAll(string(raw), "\x00", " ")
		for _, socket := range sockets {
			if socket != "" && strings.Contains(cmdline, socket) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

// stopProcesses terminates pids, escalating to SIGKILL after grace.
func stopProcesses(grace time.Duration, pids ...int) error {
	live := make([]int, 0, len(pids))
	for _, pid := range pids {
		if !processAlive(pid) {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("send SIGTERM to pid %d: %w", pid, err)
		}
		live = append(live, pid)
	}
	if len(live) == 0 {
		return nil
	}
	if waitProcessesGone(live, grace) {
		return nil
	}
	for _, pid := range live {
		if processAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if waitProcessesGone(live, 5*time.Second) {
		return nil
	}
	return fmt.Errorf("processes did not exit: %v", live)
}

func waitProcessesGone(pids []int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		alive := false
		for _, pid := range pids {
			if processAlive(pid) {
				alive = true
				break
			}
		}
		if !alive {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// killVMsBySocket terminates any VMM referencing socket and waits for it to go
// away. It is used when the SDK's graceful stop does not take effect.
func killVMsBySocket(ctx context.Context, socket string) error {
	pids := findProcessesBySocket(socket)
	if len(pids) == 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return stopProcesses(10*time.Second, pids...)
}
