package vm

import (
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

	if _, err := io.Copy(out, in); err != nil {
		return false, fmt.Errorf("copy rootfs image: %w", err)
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
