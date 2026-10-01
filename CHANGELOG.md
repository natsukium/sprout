# Changelog

Notable changes per release, for someone upgrading a pinned flake input.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
`sprout` is in its 0.x.y pre-release line, so the CLI and Nix options may
change between releases (see
[compatibility and release policy](docs/reference/compatibility.md)).

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
