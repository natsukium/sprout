//go:build !linux

package main

import "fmt"

func checkKVM(path string) (string, error) {
	return "", fmt.Errorf("%s: KVM exists only on Linux", path)
}

var hostChecks []doctorCheck
