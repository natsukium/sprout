# Compatibility and release policy

`sprout` is in the 0.x.y pre-release line. The host platform supported by this
line is Apple Silicon macOS with an `aarch64-linux` Nix builder. Other host
architectures and guests are not part of the supported contract yet.

During the 0.x series, the CLI grammar, Nix options, and generated guest
configuration may change between releases. Pin the flake input to a release
or commit when a project needs a reproducible toolchain, and upgrade sprout
together with its project configuration.

The generated `manifest.json` and each `instance.json` carry an explicit
schema version. This release writes and reads schema version 1. An unsupported
version is rejected before a VM is started; remove the affected instance with
`sprout delete -i ID --force`, then run `sprout up` after upgrading. VM `/var` data
is not migrated by sprout, so
back up important application data before deleting an instance or changing a
guest definition.

The version alone does not catch every skew: a bundle built from a newer
flake can name a cache scope or credential strategy the installed binary has
no case for, which fails at `up` with the unknown value quoted rather than as
a version mismatch. Upgrade the binary to match the flake input.

The version is reported as `<version>-<revision>`, with the source revision
always appended: no build claims the bare release number. `sprout version` is
the authoritative value for the binary being executed.
