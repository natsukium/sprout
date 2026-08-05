//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func cowClone(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		out.Close()
		// The empty file O_CREATE just made would otherwise fail the fallback
		// copy's own O_EXCL create.
		os.Remove(dst)
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("%w: %v", errCoWUnsupported, err)
		}
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("closing %s: %w", dst, err)
	}
	return nil
}
