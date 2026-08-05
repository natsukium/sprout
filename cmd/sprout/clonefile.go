package main

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Every platform maps its own refusals (EXDEV, EOPNOTSUPP, …) onto this one
// error, the caller's decision being the same for all of them.
var errCoWUnsupported = errors.New("copy-on-write clone unsupported here")

const copyChunk = 4 << 20

// dst must not exist, and the CoW path needs both paths on one filesystem,
// which callers satisfy by keeping every clone inside the state tree.
func cloneFile(src, dst string) (cow bool, err error) {
	err = cowClone(src, dst)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, errCoWUnsupported) {
		return false, err
	}
	if err := copySparse(src, dst); err != nil {
		return false, err
	}
	return false, nil
}

// Holes are approximated rather than preserved via SEEK_DATA/SEEK_HOLE, which
// is harmless for a disk image and avoids per-platform code on a path that
// never runs on macOS, sprout's only host.
func copySparse(src, dst string) (err error) {
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
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(dst)
		}
	}()

	buf := make([]byte, copyChunk)
	var off int64
	for {
		n, readErr := in.Read(buf)
		if n > 0 {
			if !allZero(buf[:n]) {
				if _, err := out.WriteAt(buf[:n], off); err != nil {
					return err
				}
			}
			off += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	// An image ending in a hole wrote nothing past its last non-zero chunk,
	// and writes alone set a file's length, so the copy would come out short.
	if err := out.Truncate(off); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", dst, err)
	}
	return nil
}

func allZero(p []byte) bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}
