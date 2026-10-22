// Command fcghact runs the Firecracker-backed GitHub Actions runner controller.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/config"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/logging"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"

	configPath string
)

func main() {
	root := &cobra.Command{
		Use:           "fcghact",
		Short:         "GitHub Actions self-hosted runner controller on Firecracker microVMs",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       fmt.Sprintf("%s (commit %s, built %s)", version, commit, date),
		RunE:          runServe,
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "", "path to the YAML configuration file")

	root.AddCommand(
		serveCmd(),
		runCmd(),
		cleanupCmd(),
		doctorCmd(),
		versionCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "fcghact: %v\n", err)
		os.Exit(1)
	}
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the webhook server and the VM controller",
		RunE:  runServe,
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "fcghact %s (commit %s, built %s)\n", version, commit, date)
		},
	}
}

func loadConfigAndLogger() (*config.Config, *slog.Logger, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}
	log := logging.New(cfg.Log, os.Stderr)
	slog.SetDefault(log)
	return cfg, log, nil
}
