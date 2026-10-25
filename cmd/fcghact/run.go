package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/controller"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/githubapp"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/network"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/vm"
)

var (
	runRepo  string
	runOrg   string
	runPlan  string
	runJobID int64
)

func runCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a single queued workflow job in the foreground",
		Long: "Run boots one microVM for an existing queued workflow job and waits for it to finish.\n" +
			"It is intended for debugging without GitHub webhooks.",
		RunE: runJob,
	}
	cmd.Flags().StringVar(&runRepo, "repo", "", "repository in owner/name form")
	cmd.Flags().StringVar(&runOrg, "org", "", "organization (required when github.scope is org)")
	cmd.Flags().StringVar(&runPlan, "plan", "", "VM plan from the plans section (runs-on spec-<name>)")
	cmd.Flags().Int64Var(&runJobID, "job-id", 0, "workflow job ID")
	_ = cmd.MarkFlagRequired("repo")
	_ = cmd.MarkFlagRequired("job-id")
	return cmd
}

func runJob(cmd *cobra.Command, _ []string) error {
	cfg, log, err := loadConfigAndLogger()
	if err != nil {
		return err
	}
	repo, err := githubapp.ParseRepo(runRepo)
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

	apiSvc, err := githubapp.New(cfg.GitHub)
	if err != nil {
		return err
	}
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

	scope := githubapp.Scope{Repo: repo, Org: runOrg}
	status, err := apiSvc.GetJobStatus(ctx, scope, runJobID)
	if err != nil {
		return err
	}
	if status.Status != "queued" {
		return fmt.Errorf("job %d is %q; only queued jobs can be dispatched", runJobID, status.Status)
	}
	var baseDisk int64
	if info, err := os.Stat(cfg.Firecracker.RootfsImage); err == nil {
		baseDisk = info.Size()
	}
	var plan *controller.ResolvedPlan
	if runPlan != "" {
		plan, err = controller.ResolvePlan(cfg, runPlan, baseDisk)
	} else {
		plan, err = controller.ResolveDefaultPlan(cfg, baseDisk)
	}
	if err != nil {
		return err
	}
	log.Info("dispatching job manually", "job_id", runJobID, "repository", repo.String(),
		"plan", plan.Name, "job_labels", status.Labels, "runner_labels", plan.Labels)

	runnerName := fmt.Sprintf("%s-manual-%d", cfg.Runner.NamePrefix, runJobID)
	jit, err := apiSvc.GenerateJITConfig(ctx, scope, githubapp.JITRequest{
		Name:          runnerName,
		Labels:        plan.Labels,
		RunnerGroupID: cfg.Runner.RunnerGroupID,
		WorkFolder:    cfg.Runner.WorkFolder,
	})
	if err != nil {
		return err
	}

	handle, err := backend.Start(ctx, vm.Spec{
		ID:            uuid.NewString(),
		Plan:          plan.Name,
		VCPUCount:     plan.VCPUCount,
		MemSizeMiB:    plan.MemSizeMiB,
		DiskSizeBytes: plan.DiskSizeBytes,
		JobID:         runJobID,
		Repository:    repo.String(),
		RunnerName:    runnerName,
		JITConfig:     jit.Encoded,
		WorkFolder:    cfg.Runner.WorkFolder,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "microvm %s started (ip=%s runner=%s)\n", handle.ID(), handle.IP(), runnerName)

	shutdown := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Duration())
		defer cancel()
		if err := handle.Stop(shutdownCtx); err != nil {
			log.Warn("stop microvm", "error", err)
		}
	}
	defer shutdown()

	done := make(chan error, 1)
	go func() { done <- handle.Wait() }()

	hardDeadline := time.NewTimer(cfg.Runner.BootTimeout.Duration() +
		cfg.Runner.RegistrationTimeout.Duration() +
		cfg.Runner.JobTimeout.Duration())
	defer hardDeadline.Stop()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-hardDeadline.C:
			fmt.Fprintln(cmd.OutOrStdout(), "maximum lifetime reached, stopping microvm")
			shutdown()
			<-done
			return nil
		case <-ctx.Done():
			shutdown()
			<-done
			return nil
		case err := <-done:
			if err != nil {
				log.Info("microvm exited", "error", err)
			}
			return nil
		case <-ticker.C:
			st, err := apiSvc.GetJobStatus(ctx, scope, runJobID)
			if err != nil {
				log.Warn("get job status", "error", err)
				continue
			}
			switch st.Status {
			case "completed":
				fmt.Fprintf(cmd.OutOrStdout(), "job completed (conclusion=%s)\n", st.Conclusion)
				shutdown()
				<-done
				return nil
			case "in_progress":
				if st.RunnerName != "" && st.RunnerName != runnerName {
					log.Warn("job was assigned to a different runner, stopping", "assigned", st.RunnerName)
					shutdown()
					<-done
					return nil
				}
			}
		}
	}
}
