package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckKVMRejectsNonKVMDevice(t *testing.T) {
	if _, err := checkKVM("/dev/null"); err == nil {
		t.Fatal("checkKVM accepted /dev/null as a KVM device")
	} else if !strings.Contains(err.Error(), "KVM_GET_API_VERSION") {
		t.Errorf("non-KVM error %q does not name the failed KVM API probe", err)
	}
}

func TestCheckKVMReportsMissingDevice(t *testing.T) {
	if _, err := checkKVM("/nonexistent/kvm-for-test"); err == nil {
		t.Fatal("checkKVM on a missing device was accepted")
	} else if !strings.Contains(err.Error(), "kvm_intel/kvm_amd") {
		t.Errorf("missing-device error %q does not name the kvm modules", err)
	}
}

func TestCheckKVMReportsPermissionDenial(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission probe needs a non-root user; root opens anything")
	}
	denied := filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(denied, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := checkKVM(denied); err == nil {
		t.Fatal("checkKVM on an inaccessible device was accepted")
	} else if !strings.Contains(err.Error(), "kvm group") {
		t.Errorf("permission error %q does not name the kvm group fix", err)
	}
}
