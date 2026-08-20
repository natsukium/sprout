# Changelog

Notable changes per release, for someone upgrading a pinned flake input.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
`sprout` is in its 0.x.y pre-release line, so the CLI and Nix options may
change between releases (see
[compatibility and release policy](docs/reference/compatibility.md)).

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
