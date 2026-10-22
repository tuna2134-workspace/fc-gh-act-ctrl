package vm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// stateFile records everything needed to clean up a VM after a controller
// crash. It never contains the JIT config.
type stateFile struct {
	ID          string    `json:"id"`
	JobID       int64     `json:"job_id"`
	Repository  string    `json:"repository"`
	RunnerName  string    `json:"runner_name"`
	IP          string    `json:"ip"`
	Tap         string    `json:"tap"`
	SocketPath  string    `json:"socket_path"`
	ConsolePath string    `json:"console_path"`
	DiskPath    string    `json:"disk_path"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	StartedAt   time.Time `json:"started_at"`
}

const stateFileName = "vm.json"

func writeState(dir string, st *stateFile) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, stateFileName+".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, stateFileName))
}

func readState(dir string) (*stateFile, error) {
	raw, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		return nil, err
	}
	var st stateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// LockStateDir takes an exclusive advisory lock so two controllers cannot manage
// the same state directory. The returned function releases the lock.
func LockStateDir(dir string) (func() error, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	path := filepath.Join(dir, "controller.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("state dir %s is already in use by another controller: %w", dir, err)
	}
	return func() error {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return f.Close()
	}, nil
}
