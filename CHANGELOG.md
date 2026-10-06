# Changelog

Notable changes per release, for someone upgrading a pinned flake input.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
`sprout` is in its 0.x.y pre-release line, so the CLI and Nix options may
change between releases (see
[compatibility and release policy](docs/reference/compatibility.md)).

## [0.2.0](https://github.com/natsukium/sprout/compare/0.1.3...v0.2.0) - 2026-10-06

sprout now runs on Linux. `x86_64-linux` and `aarch64-linux` hosts boot a
guest of their own architecture under QEMU/KVM, with every share served by a
rootless `virtiofsd`, and the NixOS module supervises instances and the router
under systemd as the nix-darwin module does under launchd. The
[compatibility reference](docs/reference/compatibility.md) lists what each
host needs; `sprout doctor` checks it.

Flake outputs are now qualified by the host system, so upgrade the binary and
the flake's `sprout` input together; a new binary pointed at an old flake stops
before building and says what to change.

| Before | Now |
| --- | --- |
| `sproutConfigurations.<name>` | `sproutConfigurations.<system>.<name>` |
| `nixosConfigurations.sprout-<name>` | `nixosConfigurations.sprout-<system>-<name>` |

`<system>` is the host's, not the guest's: the Apple Silicon bundle is
`sproutConfigurations.aarch64-darwin.<name>`.

- With the flake-parts module, run `nix flake update sprout` and list in
  `systems` every host the project's developers use (`aarch64-darwin`,
  `aarch64-linux`, `x86_64-linux`). A `sprout.vms.<name>` definition needs no
  change unless it names an architecture, such as a `withSystem
  "aarch64-linux"` that picks guest packages.
- `lib.mkVMs` now returns one host's VMs; nest the result under each host's
  system:

  ```nix
  sproutConfigurations = sprout.inputs.nixpkgs.lib.genAttrs
    [ "aarch64-darwin" "aarch64-linux" "x86_64-linux" ]
    (system: sprout.lib.mkVMs {
      pkgs = sprout.inputs.nixpkgs.legacyPackages.${system};
      vms.dev.modules = [ ./guest.nix ];
    });
  ```

- Anything that names an output by path, such as `nix build
  .#sproutConfigurations.dev` before `sprout up --bundle`, needs the system
  inserted.
- With the nix-darwin module, updating the input and running `darwin-rebuild
  switch` is the whole migration.

`manifest.json`, `instance.json`, and `snapshot.json` move to schema version 2,
which records the host, guest, and backend. On `aarch64-darwin`, existing
bundles, instances, and snapshots keep working, and an instance record is
rewritten as version 2 on its next write. A 0.1.x binary refuses a version-2
record, so do not go back to an older `sprout` once a new one has touched an
instance. A VM process left behind by a crashed 0.1.x daemon is not reclaimed
automatically: stop it (`lsof` on its `var.img` finds it) before booting.

`sprout route serve` no longer accepts `--launchd-socket`. Jobs the nix-darwin
module generates are unaffected; a hand-written launchd plist must pass
`--activated-socket` instead.

### Added

- Qualify VM flake outputs by host system ([#23](https://github.com/natsukium/sprout/pull/23))
- Accept native Linux hosts and check KVM prerequisites ([#24](https://github.com/natsukium/sprout/pull/24))
- Open routes with xdg-open and give Linux privileged-port remedies ([#25](https://github.com/natsukium/sprout/pull/25))
- Build the sprout package from every system's flake check ([#26](https://github.com/natsukium/sprout/pull/26))
- Describe the VM backend as an explicit manifest v2 contract ([#30](https://github.com/natsukium/sprout/pull/30))
- Connect QEMU guests to the embedded network over a stream socket ([#31](https://github.com/natsukium/sprout/pull/31))
- Stop QEMU over QMP and tie its life to the daemon on Linux ([#32](https://github.com/natsukium/sprout/pull/32))
- Serve each share through a rootless virtiofsd sidecar ([#34](https://github.com/natsukium/sprout/pull/34))
- Report QEMU and sidecar memory on Linux hosts ([#38](https://github.com/natsukium/sprout/pull/38))
- Check that virtiofsd's rootless sandbox works on Linux ([#39](https://github.com/natsukium/sprout/pull/39))
- Supervise instances and the router under systemd ([#42](https://github.com/natsukium/sprout/pull/42))

### Changed

- Return deleted /var space to the host daily and soon after boot ([#22](https://github.com/natsukium/sprout/pull/22))

### Fixed

- Apply the APFS xtables repair only to Darwin-hosted guests ([#28](https://github.com/natsukium/sprout/pull/28))
- Render host-side credential scripts for each bundle's host ([#29](https://github.com/natsukium/sprout/pull/29))
- Give each instance directory its own short socket path ([#33](https://github.com/natsukium/sprout/pull/33))
- Recover from a var.img an interrupted first boot left unformatted ([#41](https://github.com/natsukium/sprout/pull/41))
- Reach the VM's control socket after /tmp cleanup removes its link ([#43](https://github.com/natsukium/sprout/pull/43))
- Put git, ssh, and ps on sprout's own PATH ([#44](https://github.com/natsukium/sprout/pull/44))
- Create var.img without No_COW so btrfs snapshots clone ([#46](https://github.com/natsukium/sprout/pull/46))

### Removed

- Remove --launchd-socket, the old name of --activated-socket ([#47](https://github.com/natsukium/sprout/pull/47))


## [0.1.3](https://github.com/natsukium/sprout/compare/0.1.2...0.1.3) - 2026-10-01

`sprout exec` no longer leaves its guest command running after Ctrl-C or a
dropped connection: the command gets SIGTERM, then SIGKILL after 5 s, when its
session ends. The guest's `DefaultTimeoutStopSec` now defaults to 10 s, so a
unit that ignores SIGTERM no longer turns every `stop` into a forced one; set
`systemd.settings.Manager.DefaultTimeoutStopSec` if a service needs longer.
`--hard` needs this release's daemon, so an instance booted by an earlier
binary refuses it until it is restarted.

### Added

- Add --hard to stop and delete to skip the guest shutdown ([#20](https://github.com/natsukium/sprout/pull/20))

### Fixed

- Force a pty when --tty is given ([#16](https://github.com/natsukium/sprout/pull/16))
- Stop the guest command when its non-pty session ends ([#18](https://github.com/natsukium/sprout/pull/18))
- Let a stuck unit still end in a clean guest poweroff ([#19](https://github.com/natsukium/sprout/pull/19))

## [0.1.2](https://github.com/natsukium/sprout/compare/0.1.1...0.1.2) - 2026-08-28

Every recorded bundle now holds a Nix GC root, so `nix store gc` can no longer
collect the build a stopped, forked, or running instance depends on. Records
written by earlier versions gain their root at their next boot.

### Fixed

- Remove partial credentials when their setup fails midway ([#10](https://github.com/natsukium/sprout/pull/10))
- Bind every lock and confirmation to the instance incarnation it named ([#11](https://github.com/natsukium/sprout/pull/11))
- Hold both instances' locks across seeding, in canonical ID order ([#12](https://github.com/natsukium/sprout/pull/12))
- Protect every recorded bundle with a Nix GC root ([#13](https://github.com/natsukium/sprout/pull/13))

## [0.1.1](https://github.com/natsukium/sprout/compare/0.1.0...0.1.1) - 2026-08-20

### Changed

- Let INFO callers skip the host resource sample ([#7](https://github.com/natsukium/sprout/pull/7))

### Fixed

- Converge concurrent up/start on the winning boot instead of failing ([#1](https://github.com/natsukium/sprout/pull/1))
- Pin instance-cache bind ordering to each guestPath's mounts ([#2](https://github.com/natsukium/sprout/pull/2))
- Name a sprout router holding the port instead of only hinting lsof ([#3](https://github.com/natsukium/sprout/pull/3))
- Surface a lost /var instead of a host-key attack, and make delete atomic ([#4](https://github.com/natsukium/sprout/pull/4))
- Tell a closed guest port apart from a daemon that went away ([#5](https://github.com/natsukium/sprout/pull/5))
- Reload instead of 502 while a fresh boot opens its port ([#6](https://github.com/natsukium/sprout/pull/6))

## [0.1.0](https://github.com/natsukium/sprout/releases/tag/0.1.0) - 2026-08-13

Initial public release.
