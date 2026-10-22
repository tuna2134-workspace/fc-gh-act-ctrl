package vm

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile creates dst as a copy of src. When reflink is requested and the
// filesystem supports FICLONE the copy is instant and copy-on-write; otherwise
// it falls back to a normal byte copy.
func cloneFile(src, dst string, reflink bool) (bool, error) {
	in, err := os.Open(src)
	if err != nil {
		return false, fmt.Errorf("open rootfs image: %w", err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return false, fmt.Errorf("stat rootfs image: %w", err)
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("create rootfs clone: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = out.Close()
			_ = os.Remove(dst)
		}
	}()

	if reflink {
		if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err == nil {
			if err := out.Sync(); err != nil {
				_ = out.Close()
				return false, fmt.Errorf("sync rootfs clone: %w", err)
			}
			success = true
			_ = out.Close()
			return true, nil
		}
		// A failed FICLONE leaves an empty file; rewind before copying.
		if err := out.Truncate(0); err != nil {
			return false, fmt.Errorf("truncate after failed reflink: %w", err)
		}
		if _, err := out.Seek(0, io.SeekStart); err != nil {
			return false, fmt.Errorf("seek after failed reflink: %w", err)
		}
	}

	if err := copySparse(in, out, info.Size()); err != nil {
		// SEEK_DATA/SEEK_HOLE may be unsupported; fall back to a byte copy.
		if truncErr := out.Truncate(0); truncErr != nil {
			return false, fmt.Errorf("truncate after failed sparse copy: %w", truncErr)
		}
		if _, seekErr := in.Seek(0, io.SeekStart); seekErr != nil {
			return false, fmt.Errorf("rewind source: %w", seekErr)
		}
		if _, seekErr := out.Seek(0, io.SeekStart); seekErr != nil {
			return false, fmt.Errorf("rewind destination: %w", seekErr)
		}
		if _, err := io.Copy(out, in); err != nil {
			return false, fmt.Errorf("copy rootfs image: %w", err)
		}
	}
	if err := out.Sync(); err != nil {
		return false, fmt.Errorf("sync rootfs copy: %w", err)
	}
	if err := out.Close(); err != nil {
		return false, fmt.Errorf("close rootfs copy: %w", err)
	}
	success = true
	return false, nil
}

// copySparse copies only allocated extents so a large sparse rootfs image does
// not materialise gigabytes of zeroes. It returns an error when the filesystem
// does not support SEEK_DATA/SEEK_HOLE.
func copySparse(src, dst *os.File, size int64) error {
	if size == 0 {
		return nil
	}
	var offset int64
	for offset < size {
		data, err := unix.Seek(int(src.Fd()), offset, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break
			}
			return err
		}
		hole, err := unix.Seek(int(src.Fd()), data, unix.SEEK_HOLE)
		if err != nil || hole <= data || hole > size {
			hole = size
		}
		if _, err := src.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := dst.Seek(data, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(dst, src, hole-data); err != nil {
			return err
		}
		offset = hole
	}
	return dst.Truncate(size)
}
