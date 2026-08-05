//go:build darwin

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// clonefile(2) is atomic against the source, which is what lets
// `snapshot --live` produce an image no worse than a power cut.
func cowClone(src, dst string) error {
	err := unix.Clonefile(src, dst, 0)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.EXDEV):
		return fmt.Errorf("%w: %v", errCoWUnsupported, err)
	default:
		return err
	}
}
