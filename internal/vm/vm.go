// Package vm starts and manages Firecracker microVMs.
package vm

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Spec describes one disposable runner VM.
type Spec struct {
	// ID is the VM identifier; a UUID is generated when empty.
	ID string
	// Plan is the configured plan name, for logs.
	Plan string
	// VCPUCount and MemSizeMiB override the backend defaults when positive.
	VCPUCount  int64
	MemSizeMiB int64
	// DiskSizeBytes grows the cloned rootfs to this size when larger than the
	// base image. The guest agent expands the filesystem at boot.
	DiskSizeBytes int64
	// JobID is the GitHub Actions workflow job ID, for logs and correlation.
	JobID int64
	// Repository is owner/name.
	Repository string
	// RunnerName is the name registered with GitHub.
	RunnerName string
	// JITConfig is the single-use runner configuration from GitHub.
	JITConfig string
	// WorkFolder is where the runner keeps checkouts inside the guest.
	WorkFolder string
}

func (s Spec) validate() error {
	switch {
	case s.JobID <= 0:
		return fmt.Errorf("vm spec: job id is required")
	case s.RunnerName == "":
		return fmt.Errorf("vm spec: runner name is required")
	case s.JITConfig == "":
		return fmt.Errorf("vm spec: jit config is required")
	case s.Repository == "":
		return fmt.Errorf("vm spec: repository is required")
	case strings.ContainsAny(s.ID, `/\`):
		return fmt.Errorf("vm spec: invalid id %q", s.ID)
	case s.VCPUCount < 0:
		return fmt.Errorf("vm spec: negative vcpu count")
	case s.MemSizeMiB < 0:
		return fmt.Errorf("vm spec: negative memory size")
	case s.DiskSizeBytes < 0:
		return fmt.Errorf("vm spec: negative disk size")
	}
	return nil
}

// Metadata is handed to the guest agent through Firecracker MMDS.
type Metadata struct {
	JobID      int64     `json:"job_id"`
	Repository string    `json:"repository"`
	RunnerName string    `json:"runner_name"`
	WorkFolder string    `json:"work_folder"`
	JITConfig  string    `json:"jit_config"`
	DNS        []string  `json:"dns,omitempty"`
	IssuedAt   time.Time `json:"issued_at"`
}

// metadataEnvelope is the MMDS data store layout expected by guests. The
// documented guest path /latest/meta-data/ resolves to latest.meta-data, so a
// flat object would produce a 404.
type metadataEnvelope struct {
	Latest struct {
		MetaData Metadata `json:"meta-data"`
	} `json:"latest"`
}

func newMetadataEnvelope(meta Metadata) metadataEnvelope {
	var env metadataEnvelope
	env.Latest.MetaData = meta
	return env
}

// Handle is a running microVM.
type Handle interface {
	ID() string
	IP() string
	RunnerName() string
	StartedAt() time.Time
	// ConsolePath is the serial console log on the host. It is empty when the
	// backend does not expose one.
	ConsolePath() string
	// Wait blocks until the VMM has exited and all resources are released.
	Wait() error
	// Stop asks the VMM to exit, escalating to SIGKILL, and waits for cleanup.
	Stop(ctx context.Context) error
}

// Backend creates microVMs. Start's context only bounds startup; it may be
// cancelled after Start returns without affecting the running VM.
type Backend interface {
	Start(ctx context.Context, spec Spec) (Handle, error)
	// Reap cleans up state left behind by a previous controller process.
	Reap(ctx context.Context) error
	// Shutdown stops all VMs started by this backend.
	Shutdown(ctx context.Context) error
}
