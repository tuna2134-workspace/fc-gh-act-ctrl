// Package oci distributes the guest rootfs image as an OCI artifact.
//
// The image contains a single gzipped tar layer with one entry, rootfs.ext4.
// Push and pull are implemented with go-containerregistry so the controller
// does not depend on the docker or crane CLIs at runtime.
package oci

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"golang.org/x/sys/unix"
)

const (
	// rootfsEntry is the only file extracted from the layer.
	rootfsEntry = "rootfs.ext4"
	// layerMediaType is the expected layer type (tar+gzip).
	layerMediaType = types.OCILayer
)

// Options configures Push and Pull.
type Options struct {
	// Reference is the OCI image reference, for example
	// ghcr.io/owner/fcghact-rootfs:v1.
	Reference string
	// CacheDir stores downloaded layers. Required by Pull.
	CacheDir string
	// Token is an optional registry bearer token. When empty the Docker
	// keychain (~/.docker/config.json) is used.
	Token    string
	Insecure bool
	Logger   *slog.Logger

	// Push-only fields.
	Platform    string            // defaults to runtime.GOARCH
	Annotations map[string]string // extra manifest annotations
	TempDir     string            // where the tar is built; defaults to os.TempDir()
	Source      string            // org.opencontainers.image.source
	Version     string            // org.opencontainers.image.version
}

// Result describes a resolved or downloaded rootfs image.
type Result struct {
	Path        string `json:"path,omitempty"`
	ImageDigest string `json:"image_digest"`
	LayerDigest string `json:"layer_digest"`
	LayerSize   int64  `json:"layer_size"`
	Cached      bool   `json:"cached"`
}

type metadata struct {
	Reference   string    `json:"reference"`
	ImageDigest string    `json:"image_digest"`
	LayerDigest string    `json:"layer_digest"`
	Size        int64     `json:"size"`
	PulledAt    time.Time `json:"pulled_at"`
}

// Pull resolves ref, downloads the rootfs layer when it is not cached, and
// returns the local ext4 path.
func Pull(ctx context.Context, opts Options) (*Result, error) {
	if strings.TrimSpace(opts.CacheDir) == "" {
		return nil, errors.New("oci: cache directory is required")
	}
	res, layer, err := resolve(ctx, opts)
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(opts.CacheDir, "sha256-"+layerDigestHex(res.LayerDigest))
	path := filepath.Join(dir, rootfsEntry)
	res.Path = path

	if cached(path, res) {
		res.Cached = true
		return res, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("oci: create cache dir: %w", err)
	}

	unlock, err := lockFile(filepath.Join(opts.CacheDir, "sha256-"+layerDigestHex(res.LayerDigest)+".lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Another process may have completed the download while we waited.
	if cached(path, res) {
		res.Cached = true
		return res, nil
	}

	size, err := download(layer, path)
	if err != nil {
		return nil, fmt.Errorf("oci: download %s: %w", opts.Reference, err)
	}
	meta := metadata{
		Reference:   opts.Reference,
		ImageDigest: res.ImageDigest,
		LayerDigest: res.LayerDigest,
		Size:        size,
		PulledAt:    time.Now().UTC(),
	}
	if err := writeMetadata(dir, &meta); err != nil && opts.Logger != nil {
		opts.Logger.Warn("write rootfs metadata", "error", err)
	}
	return res, nil
}

// Inspect resolves the manifest without downloading the layer.
func Inspect(ctx context.Context, opts Options) (*Result, error) {
	res, _, err := resolve(ctx, opts)
	if err != nil {
		return nil, err
	}
	if opts.CacheDir != "" {
		path := filepath.Join(opts.CacheDir, "sha256-"+layerDigestHex(res.LayerDigest), rootfsEntry)
		res.Path = path
		res.Cached = cached(path, res)
	}
	return res, nil
}

// Push builds a single-layer OCI image from rootfsPath and uploads it.
func Push(ctx context.Context, rootfsPath string, opts Options) (*Result, error) {
	info, err := os.Stat(rootfsPath)
	if err != nil {
		return nil, fmt.Errorf("oci: stat rootfs: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("oci: rootfs %s is not a regular file", rootfsPath)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("oci: rootfs %s is empty", rootfsPath)
	}

	tempDir := opts.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	tarFile, err := os.CreateTemp(tempDir, "fcghact-rootfs-*.tar")
	if err != nil && tempDir != os.TempDir() {
		// The rootfs directory may not be writable by the pushing user.
		tarFile, err = os.CreateTemp("", "fcghact-rootfs-*.tar")
	}
	if err != nil {
		return nil, fmt.Errorf("oci: create temporary tar: %w", err)
	}
	tarPath := tarFile.Name()
	defer os.Remove(tarPath)

	if err := writeTar(tarFile, rootfsPath, info); err != nil {
		_ = tarFile.Close()
		return nil, err
	}

	layer, err := tarball.LayerFromFile(tarPath,
		tarball.WithMediaType(layerMediaType),
		tarball.WithCompressionLevel(gzip.BestSpeed),
	)
	if err != nil {
		return nil, fmt.Errorf("oci: build layer: %w", err)
	}

	platform := opts.Platform
	if platform == "" {
		platform = runtime.GOARCH
	}
	created := info.ModTime().UTC()
	img, err := mutate.Append(empty.Image, mutate.Addendum{Layer: layer})
	if err != nil {
		return nil, fmt.Errorf("oci: append layer: %w", err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)

	cfgFile, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("oci: read config: %w", err)
	}
	cfgFile.Architecture = platform
	cfgFile.OS = "linux"
	cfgFile.Created = v1.Time{Time: created}
	if cfgFile.Config.Labels == nil {
		cfgFile.Config.Labels = make(map[string]string)
	}
	cfgFile.Config.Labels["org.opencontainers.image.title"] = "fcghact-rootfs"
	img, err = mutate.ConfigFile(img, cfgFile)
	if err != nil {
		return nil, fmt.Errorf("oci: set config: %w", err)
	}

	annotations := map[string]string{
		"org.opencontainers.image.title":   "fcghact rootfs",
		"org.opencontainers.image.created": created.Format(time.RFC3339),
	}
	if opts.Source != "" {
		annotations["org.opencontainers.image.source"] = opts.Source
	}
	if opts.Version != "" {
		annotations["org.opencontainers.image.version"] = opts.Version
	}
	for k, v := range opts.Annotations {
		annotations[k] = v
	}
	img = mutate.Annotations(img, annotations).(v1.Image)

	ref, err := parseReference(opts.Reference, opts.Insecure)
	if err != nil {
		return nil, err
	}
	if err := remote.Write(ref, img, remoteOptions(ctx, opts)...); err != nil {
		return nil, fmt.Errorf("oci: push %s: %w", opts.Reference, err)
	}

	imageDigest, err := img.Digest()
	if err != nil {
		return nil, err
	}
	layerDigest, err := layer.Digest()
	if err != nil {
		return nil, err
	}
	layerSize, err := layer.Size()
	if err != nil {
		return nil, err
	}
	return &Result{
		ImageDigest: imageDigest.String(),
		LayerDigest: layerDigest.String(),
		LayerSize:   layerSize,
	}, nil
}

// Prune removes cached layers other than keepLayerDigest.
func Prune(cacheDir, keepLayerDigest string) (int, error) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	keep := "sha256-" + layerDigestHex(keepLayerDigest)
	removed := 0
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "sha256-") || entry.Name() == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(cacheDir, entry.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

func resolve(ctx context.Context, opts Options) (*Result, v1.Layer, error) {
	ref, err := parseReference(opts.Reference, opts.Insecure)
	if err != nil {
		return nil, nil, err
	}
	img, err := remote.Image(ref, remoteOptions(ctx, opts)...)
	if err != nil {
		return nil, nil, fmt.Errorf("oci: resolve %s: %w", opts.Reference, err)
	}
	imageDigest, err := img.Digest()
	if err != nil {
		return nil, nil, fmt.Errorf("oci: image digest: %w", err)
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, nil, fmt.Errorf("oci: layers: %w", err)
	}
	if len(layers) != 1 {
		return nil, nil, fmt.Errorf("oci: expected exactly one rootfs layer in %s, found %d", opts.Reference, len(layers))
	}
	layer := layers[0]
	layerDigest, err := layer.Digest()
	if err != nil {
		return nil, nil, fmt.Errorf("oci: layer digest: %w", err)
	}
	layerSize, err := layer.Size()
	if err != nil {
		return nil, nil, fmt.Errorf("oci: layer size: %w", err)
	}
	return &Result{
		ImageDigest: imageDigest.String(),
		LayerDigest: layerDigest.String(),
		LayerSize:   layerSize,
	}, layer, nil
}

func parseReference(ref string, insecure bool) (name.Reference, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("oci: image reference is required")
	}
	opts := []name.Option{}
	if insecure {
		opts = append(opts, name.Insecure)
	}
	parsed, err := name.ParseReference(ref, opts...)
	if err != nil {
		return nil, fmt.Errorf("oci: invalid reference %q: %w", ref, err)
	}
	return parsed, nil
}

func remoteOptions(ctx context.Context, opts Options) []remote.Option {
	out := []remote.Option{remote.WithContext(ctx)}
	if opts.Token != "" {
		out = append(out, remote.WithAuth(&authn.Bearer{Token: opts.Token}))
		return out
	}
	return append(out, remote.WithAuthFromKeychain(authn.DefaultKeychain))
}

// writeTar writes a single-file tar that tarball.LayerFromFile can compress.
func writeTar(f *os.File, rootfsPath string, info os.FileInfo) error {
	src, err := os.Open(rootfsPath)
	if err != nil {
		return fmt.Errorf("oci: open rootfs: %w", err)
	}
	defer src.Close()

	tw := tar.NewWriter(f)
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     rootfsEntry,
		Mode:     0o644,
		Size:     info.Size(),
		ModTime:  info.ModTime(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("oci: write tar header: %w", err)
	}
	if _, err := io.Copy(tw, src); err != nil {
		return fmt.Errorf("oci: write tar body: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("oci: close tar: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("oci: sync tar: %w", err)
	}
	return f.Close()
}

// download extracts rootfs.ext4 from the layer into dst, verifying the layer
// digest, and returns the extracted size.
func download(layer v1.Layer, dst string) (int64, error) {
	wantDigest, err := layer.Digest()
	if err != nil {
		return 0, err
	}
	mediaType, err := layer.MediaType()
	if err != nil {
		return 0, err
	}
	rc, err := layer.Compressed()
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	hasher := sha256.New()
	buffered := bufio.NewReader(io.TeeReader(rc, hasher))

	var tarReader *tar.Reader
	switch mediaType {
	case types.OCILayer, types.DockerLayer:
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			return 0, fmt.Errorf("open gzip layer: %w", err)
		}
		defer gz.Close()
		tarReader = tar.NewReader(gz)
	case types.OCIUncompressedLayer, types.DockerUncompressedLayer:
		tarReader = tar.NewReader(buffered)
	default:
		// Some registries rewrite media types; sniff the gzip magic instead.
		magic, _ := buffered.Peek(2)
		if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
			gz, err := gzip.NewReader(buffered)
			if err != nil {
				return 0, fmt.Errorf("open gzip layer: %w", err)
			}
			defer gz.Close()
			tarReader = tar.NewReader(gz)
		} else {
			tarReader = tar.NewReader(buffered)
		}
	}

	tmp := dst + ".tmp"
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	cleanup := func() {
		_ = out.Close()
		_ = os.Remove(tmp)
	}

	var size int64
	found := false
	for {
		hdr, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return 0, fmt.Errorf("read tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != rootfsEntry {
			continue
		}
		if found {
			cleanup()
			return 0, fmt.Errorf("layer contains multiple %s entries", rootfsEntry)
		}
		if hdr.Size <= 0 {
			cleanup()
			return 0, fmt.Errorf("%s is empty", rootfsEntry)
		}
		size = hdr.Size
		if _, err := io.Copy(out, tarReader); err != nil {
			cleanup()
			return 0, fmt.Errorf("extract rootfs: %w", err)
		}
		found = true
	}
	if !found {
		cleanup()
		return 0, fmt.Errorf("layer does not contain %s", rootfsEntry)
	}
	// Consume the rest of the compressed stream so the digest covers everything.
	if _, err := io.Copy(io.Discard, buffered); err != nil {
		cleanup()
		return 0, fmt.Errorf("drain layer: %w", err)
	}
	if err := out.Sync(); err != nil {
		cleanup()
		return 0, err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	got := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if got != wantDigest.String() {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("layer digest mismatch: got %s, want %s", got, wantDigest)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if dir, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return size, nil
}

func cached(path string, res *Result) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	meta, err := readMetadata(filepath.Dir(path))
	if err != nil {
		return false
	}
	return meta.LayerDigest == res.LayerDigest && meta.Size == info.Size()
}

func writeMetadata(dir string, meta *metadata) error {
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "metadata.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "metadata.json"))
}

func readMetadata(dir string) (*metadata, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return nil, err
	}
	var meta metadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

func layerDigestHex(digest string) string {
	_, hexPart, ok := strings.Cut(digest, ":")
	if !ok {
		return digest
	}
	return hexPart
}

// lockFile takes an exclusive advisory lock so concurrent pulls of the same
// layer do not race on the cache directory.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("oci: open lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("oci: lock cache: %w", err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}
