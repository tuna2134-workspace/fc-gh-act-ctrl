package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/vishvananda/netlink"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/githubapp"
)

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check host prerequisites and GitHub App credentials",
		RunE:  runDoctor,
	}
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	cfg, _, err := loadConfigAndLogger()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	failures := 0

	report := func(name string, err error, detail string) {
		if err != nil {
			failures++
			fmt.Fprintf(out, "FAIL  %-28s %v\n", name, err)
			return
		}
		if detail != "" {
			fmt.Fprintf(out, "ok    %-28s %s\n", name, detail)
			return
		}
		fmt.Fprintf(out, "ok    %-28s\n", name)
	}

	if path, err := exec.LookPath(cfg.Firecracker.Binary); err != nil {
		report("firecracker binary", err, "")
	} else {
		report("firecracker binary", nil, path)
	}
	_, kernelErr := os.Stat(cfg.Firecracker.KernelImage)
	report("kernel image", kernelErr, cfg.Firecracker.KernelImage)
	_, rootfsErr := os.Stat(cfg.Firecracker.RootfsImage)
	report("rootfs image", rootfsErr, cfg.Firecracker.RootfsImage)

	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		report("/dev/kvm", err, "")
	} else {
		_ = f.Close()
		report("/dev/kvm", nil, "read-write")
	}

	if link, err := netlink.LinkByName(cfg.Network.Bridge); err != nil {
		report("bridge", err, cfg.Network.Bridge)
	} else {
		report("bridge", nil, fmt.Sprintf("%s (index %d)", cfg.Network.Bridge, link.Attrs().Index))
	}

	if _, err := cfg.GitHub.WebhookSecretBytes(); err != nil {
		report("webhook secret", err, "")
	} else {
		report("webhook secret", nil, "")
	}
	if _, err := cfg.GitHub.PrivateKeyBytes(); err != nil {
		report("app private key", err, "")
	} else {
		report("app private key", nil, "")
	}

	probe := filepath.Join(cfg.Firecracker.StateDir, ".doctor-probe")
	err = os.MkdirAll(cfg.Firecracker.StateDir, 0o750)
	if err == nil {
		err = os.WriteFile(probe, []byte("ok"), 0o600)
		_ = os.Remove(probe)
	}
	report("state directory", err, cfg.Firecracker.StateDir)

	apiSvc, err := githubapp.New(cfg.GitHub)
	if err != nil {
		report("github app", err, "")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		slug, err := apiSvc.Check(ctx)
		cancel()
		report("github app", err, slug)
	}

	if failures > 0 {
		return fmt.Errorf("%d check(s) failed", failures)
	}
	fmt.Fprintln(out, "\nall checks passed")
	return nil
}
