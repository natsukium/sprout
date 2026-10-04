package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pretendHost(t *testing.T, host string) {
	t.Helper()
	prev := runningHostSystem
	runningHostSystem = func() (string, error) { return host, nil }
	t.Cleanup(func() { runningHostSystem = prev })
}

func otherGuest(p Platform) Platform {
	if p.GuestSystem == "x86_64-linux" {
		p.GuestSystem = "aarch64-linux"
	} else {
		p.GuestSystem = "x86_64-linux"
	}
	return p
}

func otherBackend(p Platform) Platform {
	if p.Backend == "vfkit" {
		p.Backend = "qemu"
	} else {
		p.Backend = "vfkit"
	}
	return p
}

func otherHost(p Platform) Platform {
	if p.HostSystem == "aarch64-linux" {
		p.HostSystem = "x86_64-linux"
	} else {
		p.HostSystem = "aarch64-linux"
	}
	return p
}

type platformMismatch struct {
	what     string
	recorded func(Platform) Platform
	wantErr  string
}

var platformMismatches = []platformMismatch{
	{"different guest architecture", otherGuest, "guest disk"},
	{"different backend", otherBackend, "backend"},
	{"different host", otherHost, "host"},
	{"no recorded platform", func(Platform) Platform { return Platform{} }, "does not say which host, guest, and backend"},
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertAbsent(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists (stat err %v): %s", path, err, why)
	}
}

// A refused `up` must leave everything as it found it: the running VM still
// serving, the record still describing the disk, no claim taken, no
// credentials swept.
func TestPrepareUpBootRefusesAPlatformChangeBeforeTouchingTheInstance(t *testing.T) {
	for i, c := range platformMismatches {
		t.Run(c.what, func(t *testing.T) {
			root := shortStateRoot(t)
			id := []string{"cccc00000001", "cccc00000002", "cccc00000003", "cccc00000004"}[i]
			cleanupSocketDir(t, id)
			dir := filepath.Join(root, "sprout", "instances", id)
			tok, err := publishAttempt(id, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer tok.Close()

			bundle := filepath.Join(root, "bundle")
			if err := os.MkdirAll(bundle, 0o700); err != nil {
				t.Fatal(err)
			}
			m := bootManifest(t)
			if err := writeJSON(instanceRecordPath(dir), &Instance{
				ID: id, Name: "feature", KeySource: "directory", Bundle: "/nix/store/old-bundle",
				Platform: c.recorded(m.platform()),
			}); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, varImagePath(dir), "disk")
			secret := filepath.Join(credentialsDir(dir), "token")
			mustWrite(t, secret, "s3cret")
			record := readFileString(t, instanceRecordPath(dir))
			(&fakeDaemon{}).serve(t, daemonControlSocket(t, dir))

			keepToken := false
			_, _, err = prepareUpBoot(upIdentity(root, id), dir, tok, "dev", bundle, m, 0, &keepToken)
			if err == nil {
				t.Fatal("prepareUpBoot accepted an instance created under another platform")
			}
			if !strings.Contains(err.Error(), c.wantErr) || !strings.Contains(err.Error(), "sprout delete -i feature") {
				t.Errorf("error = %q, want it to mention %q and the delete remedy", err, c.wantErr)
			}
			if got := readFileString(t, instanceRecordPath(dir)); got != record {
				t.Errorf("record rewritten by a refused up:\n%s", got)
			}
			if !instanceRunning(id) {
				t.Error("the running daemon stopped answering after a refused up")
			}
			if _, err := os.Stat(secret); err != nil {
				t.Errorf("credentials swept by a refused up: %v", err)
			}
			assertAbsent(t, filepath.Join(dir, "daemon.lock"), "a refused up claimed the instance")
		})
	}
}

func TestPrepareUpBootRefusesADiskWithoutAReadableRecord(t *testing.T) {
	for _, c := range []struct {
		what   string
		record string
	}{
		{"missing record", ""},
		{"corrupt record", "{not json"},
	} {
		t.Run(c.what, func(t *testing.T) {
			root := shortStateRoot(t)
			const id = "cccc00000010"
			cleanupSocketDir(t, id)
			dir := filepath.Join(root, "sprout", "instances", id)
			tok, err := publishAttempt(id, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer tok.Close()
			mustWrite(t, varImagePath(dir), "disk")
			if c.record != "" {
				mustWrite(t, instanceRecordPath(dir), c.record)
			}

			keepToken := false
			_, _, err = prepareUpBoot(upIdentity(root, id), dir, tok, "dev", filepath.Join(root, "bundle"), bootManifest(t), 0, &keepToken)
			if err == nil || !strings.Contains(err.Error(), "has a disk but no readable record") {
				t.Fatalf("error = %v, want the disk-without-record refusal", err)
			}
			if c.record == "" {
				assertAbsent(t, instanceRecordPath(dir), "a refused up adopted the disk under a new record")
			} else if got := readFileString(t, instanceRecordPath(dir)); got != c.record {
				t.Errorf("corrupt record replaced: %q", got)
			}
		})
	}
}

func TestPrepareUpBootRecordsTheBundlePlatform(t *testing.T) {
	root := shortStateRoot(t)
	const id = "cccc00000020"
	cleanupSocketDir(t, id)
	dir := filepath.Join(root, "sprout", "instances", id)
	tok, err := publishAttempt(id, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tok.Close()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}

	m := bootManifest(t)
	keepToken := false
	lock, _, err := prepareUpBoot(upIdentity(root, id), dir, tok, "dev", bundle, m, 0, &keepToken)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	rec := readRecord(t, dir)
	if rec.Version != instanceSchemaVersion || rec.Platform != m.platform() {
		t.Errorf("record = version %d platform %+v, want version %d platform %+v", rec.Version, rec.Platform, instanceSchemaVersion, m.platform())
	}
}

// Existing Darwin instances keep working without a delete: their record is
// read as the only platform that ever wrote it and rewritten as v2 by the
// boot that accepts it.
func TestUpAdoptsALegacyDarwinRecord(t *testing.T) {
	pretendHost(t, "aarch64-darwin")
	root := shortStateRoot(t)
	const id = "cccc00000030"
	cleanupSocketDir(t, id)
	dir := filepath.Join(root, "sprout", "instances", id)
	tok, err := publishAttempt(id, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tok.Close()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, instanceRecordPath(dir), `{"version":1,"id":"`+id+`","name":"feature","keySource":"directory","bundle":"/nix/store/old"}`)
	mustWrite(t, varImagePath(dir), "disk")

	m, err := parseManifest(encodeDoc(t, v2ManifestDoc("aarch64-darwin", "vfkit")), "aarch64-darwin")
	if err != nil {
		t.Fatal(err)
	}
	keepToken := false
	lock, _, err := prepareUpBoot(upIdentity(root, id), dir, tok, "dev", bundle, m, 0, &keepToken)
	if err != nil {
		t.Fatalf("legacy darwin record refused: %v", err)
	}
	defer lock.Close()
	want := Platform{HostSystem: "aarch64-darwin", GuestSystem: "aarch64-linux", Backend: "vfkit"}
	if rec := readRecord(t, dir); rec.Version != 2 || rec.Platform != want {
		t.Errorf("record = version %d platform %+v, want v2 %+v", rec.Version, rec.Platform, want)
	}
}

func TestLegacyInstanceRecordMigration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	const id = "cccc00000040"
	dir := filepath.Join(root, "sprout", "instances", id)
	mustWrite(t, instanceRecordPath(dir), `{"version":1,"id":"`+id+`","name":"feature"}`)

	t.Run("read as vfkit on aarch64-darwin", func(t *testing.T) {
		pretendHost(t, "aarch64-darwin")
		inst, _, err := loadInstance(id)
		if err != nil {
			t.Fatal(err)
		}
		want := Platform{HostSystem: "aarch64-darwin", GuestSystem: "aarch64-linux", Backend: "vfkit"}
		if inst.Platform != want || inst.Version != instanceSchemaVersion {
			t.Fatalf("migrated record = version %d %+v, want v2 %+v", inst.Version, inst.Platform, want)
		}
		if err := writeJSON(filepath.Join(t.TempDir(), "instance.json"), inst); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refused on linux with the delete remedy", func(t *testing.T) {
		pretendHost(t, "x86_64-linux")
		inst, _, err := loadInstance(id)
		if err != nil {
			t.Fatalf("a legacy record must stay readable for list and delete: %v", err)
		}
		bundle := Platform{HostSystem: "x86_64-linux", GuestSystem: "x86_64-linux", Backend: "qemu"}
		err = checkInstancePlatform("feature", inst.Platform, bundle)
		if err == nil || !strings.Contains(err.Error(), "sprout delete -i feature") {
			t.Fatalf("legacy record on linux: error = %v, want the delete remedy", err)
		}
	})
}

// start boots the recorded bundle; a refusal there must leave the record as
// it was, observed through a legacy symlinked bundle path that the pin step
// would otherwise rewrite.
func TestStartRefusesAPlatformChangeBeforeRewritingTheRecord(t *testing.T) {
	for i, c := range platformMismatches {
		t.Run(c.what, func(t *testing.T) {
			root := shortStateRoot(t)
			id := []string{"dddd00000001", "dddd00000002", "dddd00000003", "dddd00000004"}[i]
			cleanupSocketDir(t, id)
			dir := filepath.Join(root, "sprout", "instances", id)
			bundle := filepath.Join(root, "bundle")
			if err := os.MkdirAll(bundle, 0o700); err != nil {
				t.Fatal(err)
			}
			writeHostManifest(t, bundle)
			legacyLink := filepath.Join(root, "result")
			if err := os.Symlink(bundle, legacyLink); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(instanceRecordPath(dir), &Instance{
				ID: id, Name: "feature", KeySource: "directory", Bundle: legacyLink,
				Platform: c.recorded(hostPlatform(t)),
			}); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, varImagePath(dir), "disk")
			record := readFileString(t, instanceRecordPath(dir))

			err := startForeground(&Identity{ID: id, Name: "feature"})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("start error = %v, want it to mention %q", err, c.wantErr)
			}
			if got := readFileString(t, instanceRecordPath(dir)); got != record {
				t.Errorf("record rewritten by a refused start:\n%s", got)
			}
		})
	}
}

func TestStartRefusesADiskWithoutAReadableRecord(t *testing.T) {
	root := shortStateRoot(t)
	const id = "dddd00000010"
	cleanupSocketDir(t, id)
	dir := filepath.Join(root, "sprout", "instances", id)
	mustWrite(t, instanceRecordPath(dir), "{not json")
	mustWrite(t, varImagePath(dir), "disk")

	err := startForeground(&Identity{ID: id, Name: "feature"})
	if err == nil || !strings.Contains(err.Error(), "has a disk but no readable record") {
		t.Fatalf("start error = %v, want the disk-without-record refusal", err)
	}
}

func TestForkRefusesASourceFromAnotherPlatform(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t.Chdir(t.TempDir())

	srcID := "dddd00000020"
	dir := newTestInstance(t, root, srcID, "source", "seeded /var")
	pointBundleAtRealDir(t, srcID)
	inst := readRecord(t, dir)
	inst.Platform = otherGuest(inst.Platform)
	if err := writeJSON(instanceRecordPath(dir), inst); err != nil {
		t.Fatal(err)
	}

	err := cmdFork(srcID, false, "forked")
	if err == nil || !strings.Contains(err.Error(), "guest disk") {
		t.Fatalf("fork error = %v, want the guest mismatch refusal", err)
	}
	if ids, _ := instancesNamed("forked"); len(ids) != 0 {
		t.Errorf("a refused fork left instance %v behind", ids)
	}
}

func TestForkCarriesTheSourcePlatform(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t.Chdir(t.TempDir())

	srcID := "dddd00000021"
	newTestInstance(t, root, srcID, "source", "seeded /var")
	pointBundleAtRealDir(t, srcID)
	if err := cmdFork(srcID, false, "forked"); err != nil {
		t.Fatal(err)
	}
	ids, err := instancesNamed("forked")
	if err != nil || len(ids) != 1 {
		t.Fatalf("instancesNamed = %v (err %v)", ids, err)
	}
	inst, _, err := loadInstance(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if inst.Platform != hostPlatform(t) {
		t.Errorf("fork platform = %+v, want the source's %+v", inst.Platform, hostPlatform(t))
	}
}

func TestSnapshotCreateRecordsVersionAndPlatform(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	id := "eeee00000001"
	dir := newTestInstance(t, root, id, "feature", "data")
	if err := cmdSnapshotCreate(id, false, "snap"); err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal([]byte(readFileString(t, snapshotRecordPath(snapshotDir(dir, "snap")))), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Version != snapshotSchemaVersion || snap.Platform != hostPlatform(t) {
		t.Errorf("snapshot record = version %d %+v, want version %d %+v", snap.Version, snap.Platform, snapshotSchemaVersion, hostPlatform(t))
	}
}

// Restore replaces /var irrevocably, so a snapshot whose provenance cannot be
// confirmed is refused before the image is touched, while listing still shows
// it.
func TestSnapshotRestoreRefusesUnverifiableProvenance(t *testing.T) {
	foreign := func(t *testing.T) string {
		p := otherBackend(hostPlatform(t))
		data, _ := json.Marshal(Snapshot{Name: "snap", Version: snapshotSchemaVersion, Platform: p})
		return string(data)
	}
	cases := []struct {
		what    string
		record  func(t *testing.T) string
		wantErr string
	}{
		{"missing record", nil, "record is missing"},
		{"corrupt record", func(*testing.T) string { return "{not json" }, "corrupt"},
		{"unknown record version", func(*testing.T) string { return `{"version":9}` }, "version 9"},
		{"record without a platform", func(*testing.T) string { return `{"version":2,"name":"snap"}` }, "does not say"},
		{"legacy record off darwin", func(*testing.T) string { return `{"name":"snap"}` }, "predates"},
		{"snapshot from another backend", foreign, "matching guest and backend"},
	}
	for i, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			if c.what == "legacy record off darwin" {
				pretendHost(t, "x86_64-linux")
			}
			root := t.TempDir()
			t.Setenv("XDG_STATE_HOME", root)
			id := []string{"eeee00000011", "eeee00000012", "eeee00000013", "eeee00000014", "eeee00000015", "eeee00000016"}[i]
			dir := newTestInstance(t, root, id, "feature", "current /var")
			mustWrite(t, snapshotImage(dir, "snap"), "snapshot /var")
			if c.record != nil {
				mustWrite(t, snapshotRecordPath(snapshotDir(dir, "snap")), c.record(t))
			}

			err := cmdSnapshotRestore(id, true, "snap")
			if err == nil || !strings.Contains(err.Error(), c.wantErr) || !strings.Contains(err.Error(), `snapshot "snap"`) {
				t.Fatalf("restore error = %v, want it to name the snapshot and mention %q", err, c.wantErr)
			}
			if got := readFileString(t, varImagePath(dir)); got != "current /var" {
				t.Errorf("/var replaced by a refused restore: %q", got)
			}
			assertAbsent(t, varImagePath(dir)+".restoring", "a refused restore staged a copy")
			rows, err := gatherSnapshots(id)
			if err != nil || len(rows) != 1 || rows[0].Name != "snap" {
				t.Errorf("snapshot list = %v (err %v), want the unrestorable snapshot still listed", rows, err)
			}
		})
	}
}

func TestLegacySnapshotRecordRestoresOnDarwin(t *testing.T) {
	pretendHost(t, "aarch64-darwin")
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	const id = "eeee00000020"
	dir := filepath.Join(root, "sprout", "instances", id)
	mustWrite(t, instanceRecordPath(dir), `{"version":1,"id":"`+id+`","name":"feature","keySource":"directory","bundle":"/nix/store/b"}`)
	mustWrite(t, varImagePath(dir), "current /var")
	mustWrite(t, snapshotImage(dir, "snap"), "snapshot /var")
	mustWrite(t, snapshotRecordPath(snapshotDir(dir, "snap")), `{"name":"snap","bundle":"/nix/store/b"}`)

	if err := cmdSnapshotRestore(id, true, "snap"); err != nil {
		t.Fatalf("legacy snapshot refused on darwin: %v", err)
	}
	if got := readFileString(t, varImagePath(dir)); got != "snapshot /var" {
		t.Errorf("/var = %q after restore, want the snapshot's", got)
	}
}

// The detached child is handed an instance ID; with the record unreadable the
// identity it resolves is sparse, which must not read as "deleted while
// starting" when the real problem is an unattributable disk.
func TestUpReportsADiskWithoutARecordForAnIDSelector(t *testing.T) {
	withBootableHostBackend(t)
	root := shortStateRoot(t)
	const id = "ffff00000001"
	cleanupSocketDir(t, id)
	dir := filepath.Join(root, "sprout", "instances", id)
	mustWrite(t, instanceRecordPath(dir), "{not json")
	mustWrite(t, varImagePath(dir), "disk")
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHostManifest(t, bundle)

	err := cmdUp(id, "", ".", bundle, true, id)
	if err == nil || !strings.Contains(err.Error(), "has a disk but no readable record") {
		t.Fatalf("up error = %v, want the disk-without-record refusal", err)
	}
}

func hostManifestWithNetworkSocket(t *testing.T, name string) (*Manifest, []byte) {
	t.Helper()
	host := testHost(t)
	doc := v2ManifestDoc(host, defaultKindFor(host))
	doc["backend"].(map[string]any)["network"].(map[string]any)["socket"] = name
	data := encodeDoc(t, doc)
	m, err := parseManifest(data, host)
	if err != nil {
		t.Fatal(err)
	}
	return m, data
}

// A well-formed name can still overflow sun_path once joined to the socket
// directory; that must refuse the boot before a running VM is stopped.
func TestPrepareUpBootRefusesAnOverlongSocketBeforeStoppingTheVM(t *testing.T) {
	root := shortStateRoot(t)
	const id = "cccc00000050"
	cleanupSocketDir(t, id)
	dir := filepath.Join(root, "sprout", "instances", id)
	tok, err := publishAttempt(id, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tok.Close()
	m, _ := hostManifestWithNetworkSocket(t, strings.Repeat("n", 100)+".sock")
	if err := writeJSON(instanceRecordPath(dir), &Instance{
		ID: id, Name: "feature", KeySource: "directory", Bundle: "/nix/store/old-bundle", Platform: m.platform(),
	}); err != nil {
		t.Fatal(err)
	}
	record := readFileString(t, instanceRecordPath(dir))
	(&fakeDaemon{}).serve(t, daemonControlSocket(t, dir))

	keepToken := false
	_, _, err = prepareUpBoot(upIdentity(root, id), dir, tok, "dev", filepath.Join(root, "bundle"), m, 0, &keepToken)
	if err == nil || !strings.Contains(err.Error(), "AF_UNIX") {
		t.Fatalf("error = %v, want the socket path-limit refusal", err)
	}
	if got := readFileString(t, instanceRecordPath(dir)); got != record {
		t.Errorf("record rewritten by a refused up:\n%s", got)
	}
	if !instanceRunning(id) {
		t.Error("the running daemon stopped answering after a refused up")
	}
	assertAbsent(t, filepath.Join(dir, "daemon.lock"), "a refused up claimed the instance")
}

// Through a legacy symlinked bundle path, which the pin step would rewrite.
func TestStartRefusesAnOverlongSocketBeforeRewritingTheRecord(t *testing.T) {
	withBootableHostBackend(t)
	root := shortStateRoot(t)
	const id = "dddd00000030"
	cleanupSocketDir(t, id)
	dir := filepath.Join(root, "sprout", "instances", id)
	bundle := filepath.Join(root, "bundle")
	m, data := hostManifestWithNetworkSocket(t, strings.Repeat("n", 100)+".sock")
	mustWrite(t, filepath.Join(bundle, "manifest.json"), string(data))
	legacyLink := filepath.Join(root, "result")
	if err := os.Symlink(bundle, legacyLink); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(instanceRecordPath(dir), &Instance{
		ID: id, Name: "feature", KeySource: "directory", Bundle: legacyLink, Platform: m.platform(),
	}); err != nil {
		t.Fatal(err)
	}
	record := readFileString(t, instanceRecordPath(dir))

	err := startForeground(&Identity{ID: id, Name: "feature"})
	if err == nil || !strings.Contains(err.Error(), "AF_UNIX") {
		t.Fatalf("start error = %v, want the socket path-limit refusal", err)
	}
	if got := readFileString(t, instanceRecordPath(dir)); got != record {
		t.Errorf("record rewritten by a refused start:\n%s", got)
	}
}
