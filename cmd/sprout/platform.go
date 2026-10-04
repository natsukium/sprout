package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Platform is the identity a /var image is bound to: the guest's architecture
// and the backend's device and disk assumptions are baked into it, and a
// record copied from another machine names a host this one is not.
type Platform struct {
	HostSystem  string `json:"hostSystem"`
	GuestSystem string `json:"guestSystem"`
	Backend     string `json:"backend"`
}

func (p Platform) known() bool {
	return p.HostSystem != "" && p.GuestSystem != "" && p.Backend != ""
}

func (p Platform) String() string {
	return fmt.Sprintf("a %s guest under %s on %s", p.GuestSystem, p.Backend, p.HostSystem)
}

// A package-level seam so tests can stand in for another host.
var runningHostSystem = hostNixSystem

// Records from before the platform was recorded were only ever written by
// vfkit on aarch64-darwin, so there, and only there, their platform is known.
func legacyPlatform() (Platform, bool) {
	host, err := runningHostSystem()
	if err != nil || host != legacyHostSystem {
		return Platform{}, false
	}
	return Platform{HostSystem: legacyHostSystem, GuestSystem: "aarch64-linux", Backend: "vfkit"}, true
}

func (m *Manifest) platform() Platform {
	return Platform{HostSystem: m.Host.System, GuestSystem: m.Guest.System, Backend: m.contract.kind.name}
}

func checkInstancePlatform(display string, recorded, bundle Platform) error {
	remedy := fmt.Sprintf("delete it (`sprout delete -i %s`) or use another name", display)
	switch {
	case !recorded.known():
		return fmt.Errorf("instance %q has a record that does not say which host, guest, and backend created it; `sprout delete -i %s` to start over", display, display)
	case recorded.GuestSystem != bundle.GuestSystem:
		return fmt.Errorf("instance %q holds a %s guest disk; this bundle boots %s; %s", display, recorded.GuestSystem, bundle.GuestSystem, remedy)
	case recorded.Backend != bundle.Backend:
		return fmt.Errorf("instance %q was created by the %s backend; this bundle boots %s, whose device and disk assumptions differ; %s", display, recorded.Backend, bundle.Backend, remedy)
	case recorded.HostSystem != bundle.HostSystem:
		return fmt.Errorf("instance %q was created on a %s host, not this %s one; %s", display, recorded.HostSystem, bundle.HostSystem, remedy)
	}
	return nil
}

// A record that cannot be read only means "new instance" while there is no
// disk: adopting a surviving var.img under whatever bundle boots next would
// bind it to a platform nobody checked.
func loadBootRecord(id, display, dir string) (*Instance, error) {
	inst, _, err := loadInstance(id)
	if err == nil {
		return inst, nil
	}
	if _, statErr := os.Stat(varImagePath(dir)); statErr != nil {
		return nil, err
	}
	return nil, diskWithoutRecordError(display, err)
}

func diskWithoutRecordError(display string, cause error) error {
	return fmt.Errorf("instance %q has a disk but no readable record of which host, guest, and backend created it (%v); `sprout delete -i %s` to start over", display, cause, display)
}

const snapshotSchemaVersion = 2

// Read directly rather than through listSnapshots, which tolerates a broken
// record so that listing never hides an image: restore must not.
func loadSnapshotRecord(instDir, name string) (*Snapshot, error) {
	data, err := os.ReadFile(snapshotRecordPath(snapshotDir(instDir, name)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("its record is missing")
		}
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("its record is corrupt: %w", err)
	}
	switch snap.Version {
	case snapshotSchemaVersion:
	case 0:
		p, ok := legacyPlatform()
		if !ok {
			return nil, errors.New("its record predates platform tracking, which only aarch64-darwin can interpret")
		}
		snap.Platform, snap.Version = p, snapshotSchemaVersion
	default:
		return nil, fmt.Errorf("its record uses version %d, which this sprout does not support", snap.Version)
	}
	if !snap.Platform.known() {
		return nil, errors.New("its record does not say which host, guest, and backend it was taken under")
	}
	return &snap, nil
}

func checkSnapshotPlatform(display, snapName string, inst Platform, snap *Snapshot) error {
	if !inst.known() {
		return fmt.Errorf("instance %q has a record that does not say which host, guest, and backend created it, so snapshot %q cannot be checked against it; `sprout delete -i %s` to start over", display, snapName, display)
	}
	if inst != snap.Platform {
		return fmt.Errorf("snapshot %q holds %s, but instance %q is %s; restore it into an instance with the matching guest and backend, or delete it (`sprout snapshot delete -i %s %s`)", snapName, snap.Platform, display, inst, display, snapName)
	}
	return nil
}

func unrestorableSnapshotError(display, snapName string, cause error) error {
	return fmt.Errorf("snapshot %q of instance %q cannot be restored: %v; delete it (`sprout snapshot delete -i %s %s`)", snapName, display, cause, display, snapName)
}

// For callers that only need the refusal, before a sparse identity read from
// the unreadable record sends them down a misleading path.
func refuseDiskWithoutRecord(id, display string) error {
	dir, err := instanceDir(id)
	if err != nil {
		return err
	}
	if _, _, err := loadInstance(id); err != nil {
		if _, statErr := os.Stat(varImagePath(dir)); statErr == nil {
			return diskWithoutRecordError(display, err)
		}
	}
	return nil
}
