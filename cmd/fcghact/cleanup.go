package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/network"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/vm"
)

func cleanupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cleanup",
		Short: "Reap orphaned microVMs, TAP devices and state directories",
		RunE:  runCleanup,
	}
}

func runCleanup(cmd *cobra.Command, _ []string) error {
	cfg, log, err := loadConfigAndLogger()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	release, err := vm.LockStateDir(cfg.Firecracker.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()

	netMgr, err := network.New(cfg.Network)
	if err != nil {
		return err
	}
	backend, err := vm.NewFirecracker(vm.Options{
		Config:         cfg.Firecracker,
		Network:        netMgr,
		Logger:         log,
		BootTimeout:    cfg.Runner.BootTimeout.Duration(),
		SkipValidation: true,
	})
	if err != nil {
		return err
	}
	if err := backend.Reap(ctx); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "cleanup complete")
	return nil
}
