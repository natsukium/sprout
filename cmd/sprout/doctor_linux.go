//go:build linux

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// From linux/kvm.h: KVM_GET_API_VERSION is _IO(KVMIO, 0x00), and QEMU refuses
// any API version other than 12.
const (
	kvmGetAPIVersion = 0xAE00
	kvmAPIVersion    = 12
)

// Opening read-write proves the current user can hand the device to QEMU, and
// the ioctl proves the path is KVM rather than any openable file.
func checkKVM(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s not found — KVM is unavailable: enable virtualization in the BIOS, load the kvm and kvm_intel/kvm_amd modules (lsmod | grep kvm), and retry", path)
		}
		if os.IsPermission(err) {
			return "", fmt.Errorf("%s exists but is not usable by the current user (%v) — add the user to the kvm group (usermod -aG kvm, then log back in) or fix the device ACL (getfacl %s)", path, err, path)
		}
		return "", fmt.Errorf("%s is not usable (%v)", path, err)
	}
	defer f.Close()
	version, err := unix.IoctlRetInt(int(f.Fd()), kvmGetAPIVersion)
	if err != nil {
		return "", fmt.Errorf("%s did not answer KVM_GET_API_VERSION (%v) — KVM is not usable through it", path, err)
	}
	if version != kvmAPIVersion {
		return "", fmt.Errorf("%s reports KVM API version %d, but QEMU needs %d", path, version, kvmAPIVersion)
	}
	return fmt.Sprintf("%s is usable (KVM API %d)", path, version), nil
}

var hostChecks = []doctorCheck{{"rootless shares", checkRootlessShares}}

func checkRootlessShares() (string, error) {
	if virtiofsdPath == "" {
		return "", inconclusive("this sprout was built without a virtiofsd to probe with (build it with Nix); `sprout up` still checks the bundle's own virtiofsd at boot")
	}
	if err := probeVirtiofsd([]string{virtiofsdPath}); err != nil {
		return "", err
	}
	detail := "virtiofsd's namespace sandbox works (probed " + virtiofsdPath + ")"
	if v, ok := readSysctl("kernel.apparmor_restrict_unprivileged_userns"); ok && v == "1" {
		detail += "; AppArmor restricts user namespaces per executable here, so a bundle's own virtiofsd is checked again at boot"
	}
	return detail, nil
}
