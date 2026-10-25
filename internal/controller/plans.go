package controller

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/config"
)

// specLabelPrefix selects a named plan from the configuration file.
const specLabelPrefix = "spec-"

// ResolvedPlan is a concrete VM size for one job.
type ResolvedPlan struct {
	Name          string
	SpecLabel     string
	VCPUCount     int64
	MemSizeMiB    int64
	DiskSizeBytes int64
	Labels        []string
}

// SpecLabelName returns the plan requested by a spec-<name> job label.
func SpecLabelName(labels []string) (string, bool, error) {
	var name string
	found := false
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if len(label) < len(specLabelPrefix) || !strings.EqualFold(label[:len(specLabelPrefix)], specLabelPrefix) {
			continue
		}
		if found {
			return "", false, errors.New("multiple spec- labels are not allowed")
		}
		name = strings.ToLower(strings.TrimSpace(label[len(specLabelPrefix):]))
		found = true
	}
	if !found {
		return "", false, nil
	}
	if name == "" {
		return "", false, errors.New("empty plan name in spec- label")
	}
	return name, true, nil
}

// ResolvePlan resolves a named plan. Zero vCPU/memory values inherit the
// firecracker defaults; the disk is never smaller than the base rootfs image.
func ResolvePlan(cfg *config.Config, name string, baseDiskBytes int64) (*ResolvedPlan, error) {
	planCfg, ok := cfg.Plans[name]
	if !ok {
		return nil, fmt.Errorf("unknown plan %q (configured plans: %s)", name, planNames(cfg))
	}
	vcpu := planCfg.VCPUCount
	if vcpu <= 0 {
		vcpu = cfg.Firecracker.VCPUCount
	}
	mem := planCfg.MemSizeMiB
	if mem <= 0 {
		mem = cfg.Firecracker.MemSizeMiB
	}
	disk := planCfg.DiskSize.Bytes()
	if disk < baseDiskBytes {
		disk = baseDiskBytes
	}
	return &ResolvedPlan{
		Name:          name,
		SpecLabel:     specLabelPrefix + name,
		VCPUCount:     vcpu,
		MemSizeMiB:    mem,
		DiskSizeBytes: disk,
		Labels:        PlanLabels(cfg, name, vcpu, mem, disk),
	}, nil
}

// ResolveDefaultPlan returns the plan used when a job has no spec- label. A
// plan named "default" takes precedence over the global firecracker settings.
func ResolveDefaultPlan(cfg *config.Config, baseDiskBytes int64) (*ResolvedPlan, error) {
	if _, ok := cfg.Plans["default"]; ok {
		return ResolvePlan(cfg, "default", baseDiskBytes)
	}
	return &ResolvedPlan{
		VCPUCount:     cfg.Firecracker.VCPUCount,
		MemSizeMiB:    cfg.Firecracker.MemSizeMiB,
		DiskSizeBytes: baseDiskBytes,
		Labels:        PlanLabels(cfg, "", cfg.Firecracker.VCPUCount, cfg.Firecracker.MemSizeMiB, baseDiskBytes),
	}, nil
}

// PlanLabels builds the label set advertised by a runner for a plan.
func PlanLabels(cfg *config.Config, name string, vcpu, mem, disk int64) []string {
	labels := append([]string(nil), cfg.Runner.Labels...)
	if cfg.Runner.ResourceLabels {
		labels = append(labels, ResourceLabels(vcpu, mem, disk)...)
	}
	if name != "" {
		labels = append(labels, specLabelPrefix+name)
	}
	return JITLabels(labels)
}

func planNames(cfg *config.Config) string {
	if len(cfg.Plans) == 0 {
		return "none"
	}
	names := make([]string, 0, len(cfg.Plans))
	for name := range cfg.Plans {
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}
