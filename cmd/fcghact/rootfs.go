package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/config"
	"github.com/tuna2134-workspace/fc-gh-act-ctrl/internal/oci"
)

// rootfsFlags holds flags shared by the `fcghact rootfs` subcommands.
type rootfsFlags struct {
	ref      string
	cacheDir string
	token    string
	tokenEnv string
	insecure bool
	file     string
	tmpDir   string
	platform string
	source   string
	version  string
}

func (f *rootfsFlags) addCommon(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.ref, "ref", "", "OCI reference (defaults to rootfs.oci_reference)")
	cmd.Flags().StringVar(&f.cacheDir, "cache-dir", "", "cache directory (defaults to <state_dir>/images)")
	cmd.Flags().StringVar(&f.token, "token", "", "registry bearer token")
	cmd.Flags().StringVar(&f.tokenEnv, "token-env", "", "environment variable holding the registry token")
	cmd.Flags().BoolVar(&f.insecure, "insecure", false, "allow plain HTTP registries")
}

func rootfsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rootfs",
		Short: "Pull, push and inspect the OCI rootfs image",
	}
	cmd.AddCommand(rootfsPullCmd(), rootfsPushCmd(), rootfsInspectCmd())
	return cmd
}

func rootfsPullCmd() *cobra.Command {
	f := &rootfsFlags{}
	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Download the rootfs image into the local cache",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadPartial(configPath)
			if err != nil {
				return err
			}
			opts, err := f.options(cfg, false)
			if err != nil {
				return err
			}
			ctx, stop := commandContext(cmd)
			defer stop()
			res, err := oci.Pull(ctx, opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rootfs: %s\nimage:  %s\nlayer:  %s\ncached: %t\n",
				res.Path, res.ImageDigest, res.LayerDigest, res.Cached)
			return nil
		},
	}
	f.addCommon(cmd)
	return cmd
}

func rootfsInspectCmd() *cobra.Command {
	f := &rootfsFlags{}
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Resolve the rootfs image manifest without downloading it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadPartial(configPath)
			if err != nil {
				return err
			}
			opts, err := f.options(cfg, false)
			if err != nil {
				return err
			}
			ctx, stop := commandContext(cmd)
			defer stop()
			res, err := oci.Inspect(ctx, opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "image:  %s\nlayer:  %s\nsize:   %d\ncached: %t\npath:   %s\n",
				res.ImageDigest, res.LayerDigest, res.LayerSize, res.Cached, res.Path)
			return nil
		},
	}
	f.addCommon(cmd)
	return cmd
}

func rootfsPushCmd() *cobra.Command {
	f := &rootfsFlags{}
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Upload a rootfs ext4 image as an OCI artifact",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadPartial(configPath)
			if err != nil {
				return err
			}
			if f.file == "" {
				f.file = cfg.Firecracker.RootfsImage
			}
			if f.file == "" {
				return errors.New("--file is required")
			}
			opts, err := f.options(cfg, true)
			if err != nil {
				return err
			}
			ctx, stop := commandContext(cmd)
			defer stop()
			res, err := oci.Push(ctx, f.file, opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pushed %s\nimage:  %s\nlayer:  %s\nsize:   %d\n",
				opts.Reference, res.ImageDigest, res.LayerDigest, res.LayerSize)
			return nil
		},
	}
	f.addCommon(cmd)
	cmd.Flags().StringVar(&f.file, "file", "", "rootfs ext4 file (defaults to firecracker.rootfs_image)")
	cmd.Flags().StringVar(&f.tmpDir, "tmp-dir", "", "directory for the temporary tar (defaults to the rootfs directory)")
	cmd.Flags().StringVar(&f.platform, "platform", "", "image architecture (defaults to the build host)")
	cmd.Flags().StringVar(&f.source, "source", "", "value for org.opencontainers.image.source")
	cmd.Flags().StringVar(&f.version, "version", "", "value for org.opencontainers.image.version")
	return cmd
}

// options merges flags with the configuration file defaults.
func (f *rootfsFlags) options(cfg *config.Config, push bool) (oci.Options, error) {
	ref := firstNonEmpty(f.ref, cfg.Rootfs.OCIReference)
	if ref == "" {
		return oci.Options{}, errors.New("--ref or rootfs.oci_reference is required")
	}
	token := f.token
	if token == "" {
		token = cfg.Rootfs.Token
	}
	if token == "" && f.tokenEnv != "" {
		token = os.Getenv(f.tokenEnv)
	}
	if token == "" && cfg.Rootfs.TokenEnv != "" {
		token = os.Getenv(cfg.Rootfs.TokenEnv)
	}
	cacheDir := firstNonEmpty(f.cacheDir, cfg.Rootfs.CacheDir)
	if cacheDir == "" {
		cacheDir = filepath.Join(cfg.Firecracker.StateDir, "images")
	}
	opts := oci.Options{
		Reference: ref,
		CacheDir:  cacheDir,
		Token:     token,
		Insecure:  f.insecure || cfg.Rootfs.Insecure,
		Platform:  f.platform,
		Source:    f.source,
		Version:   f.version,
		TempDir:   f.tmpDir,
	}
	if push && opts.TempDir == "" && f.file != "" {
		opts.TempDir = filepath.Dir(f.file)
	}
	return opts, nil
}

// rootfsOptionsFromConfig builds OCI options for the daemon startup path.
func rootfsOptionsFromConfig(cfg *config.Config) oci.Options {
	cacheDir := cfg.Rootfs.CacheDir
	if cacheDir == "" {
		cacheDir = filepath.Join(cfg.Firecracker.StateDir, "images")
	}
	token := cfg.Rootfs.Token
	if token == "" && cfg.Rootfs.TokenEnv != "" {
		token = os.Getenv(cfg.Rootfs.TokenEnv)
	}
	return oci.Options{
		Reference: cfg.Rootfs.OCIReference,
		CacheDir:  cacheDir,
		Token:     token,
		Insecure:  cfg.Rootfs.Insecure,
	}
}

// prepareRootfs downloads the OCI rootfs (when configured) and points the
// Firecracker configuration at the cached ext4 image.
func prepareRootfs(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if cfg.Rootfs.OCIReference == "" {
		return nil
	}
	opts := rootfsOptionsFromConfig(cfg)
	res, err := oci.Pull(ctx, opts)
	if err != nil {
		return fmt.Errorf("pull rootfs image %s: %w", opts.Reference, err)
	}
	cfg.Firecracker.RootfsImage = res.Path
	log.Info("rootfs image ready",
		"reference", opts.Reference,
		"image_digest", res.ImageDigest,
		"layer_digest", res.LayerDigest,
		"path", res.Path,
		"cached", res.Cached,
	)
	if cfg.Rootfs.Prune {
		removed, err := oci.Prune(opts.CacheDir, res.LayerDigest)
		if err != nil {
			log.Warn("prune rootfs cache", "error", err)
		} else if removed > 0 {
			log.Info("pruned old rootfs images", "removed", removed)
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
