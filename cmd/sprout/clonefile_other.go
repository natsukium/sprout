//go:build !darwin && !linux

package main

func cowClone(src, dst string) error {
	return errCoWUnsupported
}
