# Compatibility and release policy

`sprout` is in the 0.x.y pre-release line.

## Supported platforms

Each host system boots exactly one guest system through exactly one backend:

| Host system | Guest system | Backend | Hypervisor | Shares |
| --- | --- | --- | --- | --- |
| `aarch64-darwin` (Apple Silicon macOS) | `aarch64-linux` | `vfkit` | Virtualization.framework | vfkit's virtio-fs device |
| `x86_64-linux` | `x86_64-linux` | `qemu` | QEMU with KVM | one rootless `virtiofsd` per share |
| `aarch64-linux` | `aarch64-linux` | `qemu` | QEMU with KVM | one rootless `virtiofsd` per share |

The guest is always Linux of the host's CPU architecture, so a Linux host
builds it natively while a Mac needs an `aarch64-linux` builder. Host-side
prerequisites differ per row; `sprout doctor` checks the ones for the host it
runs on, and the [getting-started tutorial](../tutorials/getting-started.md)
lists them.

Every command, option, and guest feature works the same on all three, except
where a page names a backend: read-only shares (enforced on the host only
under QEMU; see [what the guest can reach](../explanation/architecture.md#what-the-guest-can-reach)),
copy-on-write snapshots (a filesystem property; see
[snapshots](../how-to/snapshots.md#when-it-is-not-instant)), privileged host
ports ([route](../how-to/route.md#the-port-80-problem)), and the service
manager the host modules use (launchd on nix-darwin, systemd on NixOS; see
[run as a daemon](../how-to/run-as-daemon.md)).

How each row is tested:

- `x86_64-linux`: CI runs the end-to-end suite (`nix run .#sproutTests.all`,
  real guests under QEMU/KVM), the Go tests, and the NixOS module's VM tests.
- `aarch64-darwin`: CI runs the Go tests and the flake checks on macOS; the
  same end-to-end suite runs under vfkit on Apple Silicon outside CI, since
  hosted macOS runners cannot boot it.
- `aarch64-linux`: CI runs the Go tests and flake checks and builds a guest
  closure, but boots no guest, because GitHub's arm runners expose no KVM.

### Not supported

None of these works today. They are the current limits of what is built and
tested, not decisions against them:

- **Other hosts**: Intel Macs (`x86_64-darwin`) and any OS other than macOS
  and Linux. `sprout up` and `sprout doctor` refuse there and name the
  supported hosts.
- **A guest of another architecture**: no `x86_64-linux` guest on Apple
  Silicon (Rosetta) and no `aarch64-linux` guest on an `x86_64-linux` host
  (QEMU's software emulation). Run foreign binaries inside the guest instead;
  see [emulate a foreign architecture](../how-to/emulate-foreign-architectures.md).
- **Another backend for a host**: `vfkit` on Linux or `qemu` on macOS. The
  [`backend`](configuration.md#options) option accepts only the host's own,
  and evaluation of that host's bundle fails otherwise.
- **Linux without KVM**: QEMU runs with `-enable-kvm` and has no software
  fallback, so a host without a usable `/dev/kvm` (a VM without nested
  virtualization, for example) cannot boot guests.
- **Linux without unprivileged user namespaces**: every share is served by
  a `virtiofsd` in its own user namespace, and sprout does not fall back to
  9p or to a `virtiofsd` running as root.
- **State carried across hosts**: an instance or snapshot records the host,
  guest, and backend that made its disk, and reusing it under another
  combination is refused (see [reusing a
  disk](instance-state.md#reusing-a-disk)).

## Versioning

During the 0.x series, the CLI grammar, Nix options, and generated guest
configuration may change between releases. Pin the flake input to a release
or commit when a project needs a reproducible toolchain, and upgrade sprout
together with its project configuration.

The generated `manifest.json`, each `instance.json`, and each `snapshot.json`
carry an explicit schema version. This release writes version 2 of all three.
It still reads the earlier formats (version 1 manifests and instance records,
and snapshot records with no version field) on `aarch64-darwin`, the only
host that could produce them, as the vfkit backend it always was, so existing Darwin bundles, instances,
and snapshots keep working; elsewhere a version-1 manifest asks for a rebuild
with a sprout flake that emits version 2, and a version-1 record asks for a
delete. A newer version is rejected before a VM is started with a request to
upgrade sprout. Records also name the host, guest, and backend that created
the disk, and reusing one under a different combination is refused (see
[reusing a disk](instance-state.md#reusing-a-disk)); remove the affected
instance with `sprout delete -i ID --force`, then run `sprout up`. VM `/var`
data is not migrated by sprout, so back up important application data before
deleting an instance or changing a guest definition.

The flake output that carries the bundles is qualified by host system:
`sproutConfigurations.<system>.<name>`, with the guest exposed as
`nixosConfigurations.sprout-<system>-<name>`. A flake written for an earlier
release, and a bundle built from one, need the changes the
[changelog](../../CHANGELOG.md) lists for the release that introduced them.

The version alone does not catch every skew: a bundle built from a newer
flake can name a cache scope or credential strategy the installed binary has
no case for, which fails at `up` with the unknown value quoted rather than as
a version mismatch. Upgrade the binary to match the flake input.

Instances left running across an upgrade from 0.1.x stay reachable, so this
release can stop them. A VM process that a crashed 0.1.x daemon left behind is not
reclaimed automatically, because the socket path it was started with may
since name another state root's instance; the next boot then fails naming the
disk image the process still holds. Stop it (`lsof` on that path finds it)
and boot again.

The version is reported as `<version>-<revision>`, with the source revision
always appended: no build claims the bare release number. `sprout version` is
the authoritative value for the binary being executed.
