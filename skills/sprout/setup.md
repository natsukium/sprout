# First-time setup

Read this when `sprout up` has never run here: on a new host, or in a
repository whose `flake.nix` declares no sprout VM (a missing definition
surfaces as an `up` error naming the candidates, or `sprout status`
reporting no `flake.nix`).

## New host

`sprout doctor` checks every prerequisite `up` needs on this host and
prints the fix for whatever is missing. On macOS that is Nix with flakes,
an `aarch64-linux` builder, Virtualization.framework, and ssh; on Linux,
Nix with flakes, a usable `/dev/kvm`, a `virtiofsd` that can enter its
user-namespace sandbox (`rootless shares`), and ssh. Run it once and
apply what it says:

```sh
sprout doctor
sprout doctor --build    # additionally proves the guest builds, with a trivial Linux build
```

Several Linux fixes change the host (joining the `kvm` group, setting a
sysctl such as `kernel.apparmor_restrict_unprivileged_userns=0` on
Ubuntu) and need root. Do not apply them yourself; report the failing
check and doctor's hint to the user.

## Repository without a definition

`sprout init` writes a `flake.nix` declaring one VM (definition name
`dev`; `--vm NAME` overrides). It refuses to overwrite an existing
flake — for a repository that already has one, follow the
getting-started tutorial in the sprout docs to add the module instead.

```sh
sprout init
git add flake.nix        # Nix cannot see an unstaged new file
sprout up
```

The `git add` is not optional: in a Git checkout, Nix evaluates only
tracked or staged files, so an unstaged `flake.nix` fails the build with
a "file not found" style error.
