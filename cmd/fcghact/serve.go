package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/config"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/controller"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/githubapp"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/network"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/server"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/vm"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/webhook"
)

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, log, err := loadConfigAndLogger()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	release, err := vm.LockStateDir(cfg.Firecracker.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()

	if err := prepareRootfs(ctx, cfg, log); err != nil {
		return err
	}

	secret, err := cfg.GitHub.WebhookSecretBytes()
	if err != nil {
		return err
	}
	apiSvc, err := githubapp.New(cfg.GitHub)
	if err != nil {
		return err
	}
	checkCtx, cancelCheck := context.WithTimeout(ctx, 20*time.Second)
	if slug, err := apiSvc.Check(checkCtx); err != nil {
		log.Warn("github app authentication check failed", "error", err)
	} else {
		log.Info("github app authenticated", "slug", slug)
	}
	cancelCheck()

	netMgr, err := network.New(cfg.Network)
	if err != nil {
		return err
	}
	if err := netMgr.EnsureBridge(); err != nil {
		return fmt.Errorf("prepare network: %w", err)
	}

	backend, err := vm.NewFirecracker(vm.Options{
		Config:      cfg.Firecracker,
		Network:     netMgr,
		Logger:      log,
		BootTimeout: cfg.Runner.BootTimeout.Duration(),
	})
	if err != nil {
		return err
	}
	if err := backend.Reap(ctx); err != nil {
		log.Warn("failed to reap leftover state", "error", err)
	}

	ctrl := controller.New(cfg, apiSvc, backend, log)
	srv := server.New(server.Options{
		Config:  cfg.Server,
		Webhook: webhook.New(secret, ctrl, log),
		Ready:   readiness(cfg),
		Logger:  log,
	})

	serveErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.Server.Listen, "webhook_path", cfg.Server.WebhookPath)
		serveErr <- srv.ListenAndServe()
	}()

	ctrlDone := make(chan struct{})
	go func() {
		defer close(ctrlDone)
		ctrl.Run(ctx)
	}()

	var runErr error
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("http server: %w", err)
			stop()
		}
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Duration())
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http server shutdown", "error", err)
	}
	if err := ctrl.Shutdown(shutdownCtx); err != nil {
		log.Warn("controller shutdown", "error", err)
	}
	<-ctrlDone
	log.Info("stopped")
	return runErr
}

// readiness verifies the host prerequisites without calling GitHub on every
// probe.
func readiness(cfg *config.Config) func(context.Context) error {
	return func(context.Context) error {
		if _, err := os.Stat(cfg.Firecracker.KernelImage); err != nil {
			return fmt.Errorf("kernel image: %w", err)
		}
		if _, err := os.Stat(cfg.Firecracker.RootfsImage); err != nil {
			return fmt.Errorf("rootfs image: %w", err)
		}
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("open /dev/kvm: %w", err)
		}
		return f.Close()
	}
}
