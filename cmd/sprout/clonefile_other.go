//go:build !darwin && !linux

package main

func cowClone(src, dst string) error {
	return errCoWUnsupported
}

func markedNoCoW(string) bool { return false }
