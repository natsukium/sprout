# Getting started

Boot your first microVM, run a command inside it, open a shell, and delete the
instance.

You need Nix with flakes enabled and one of the hosts below. sprout installed
with Nix carries its own `git`, which it puts ahead of a `/usr/bin` stub, and
an OpenSSH it uses only when no `ssh` is on `PATH`; a plain `go build` relies
on both being installed. The guest is a NixOS system of the host's CPU architecture; the
[compatibility reference](../reference/compatibility.md#supported-platforms)
has the full mapping and what is out of scope.

On **macOS on Apple Silicon**:

- An `aarch64-linux` builder Nix can reach. The guest is a Linux NixOS
  closure, which a Mac cannot build natively; nixpkgs'
  [`darwin.linux-builder`](https://nixos.org/manual/nixpkgs/stable/#sec-darwin-builder)
  (`nix run nixpkgs#darwin.linux-builder`, no nix-darwin required) is the
  quickest answer, and nix-darwin's `nix.linux-builder` option runs the same
  builder as a managed service. [The examples' build
  notes](../../examples/README.md#building-on-apple-silicon) list the
  alternatives and the project binary cache. Without a builder, the first
  `sprout up` fails inside `nix build`.

On **Linux** (`x86_64` or `aarch64`):

- KVM, with `/dev/kvm` readable and writable by your user. Most
  distributions grant that to the `kvm` group (`sudo usermod -aG kvm
  "$USER"`, then log in again). Hardware virtualization must be enabled in
  the firmware, and inside a VM the hypervisor must offer nested
  virtualization.
- Unprivileged user namespaces. Every share the guest mounts (the workspace,
  the host Nix store) is served by a `virtiofsd` running as you inside its
  own user namespace. Most distributions allow this by default. Ubuntu 24.04
  and later restrict it through AppArmor: run `sudo sysctl -w
  kernel.apparmor_restrict_unprivileged_userns=0` (and persist it under
  `/etc/sysctl.d/`), or install an AppArmor profile that grants `userns` to
  the `virtiofsd` in the Nix store.
- Nothing else: the guest has the host's architecture, so Nix builds it
  locally with no remote builder, and each VM's build brings its own QEMU and
  `virtiofsd`.

## Get sprout and check the prerequisites

Get `sprout` itself on your PATH: ad hoc with `nix shell github:natsukium/sprout`,
or durably by adding the `sprout` package to your dev shell. `sprout doctor`
verifies the prerequisites above for the host it runs on and prints the fix
for anything missing. On a Mac:

```console
$ sprout doctor
✓ platform        aarch64-darwin
✓ nix             nix (Nix) 2.28.3
✓ flakes          nix-command flakes
✓ linux builder   builders = @/etc/nix/machines
✓ virtualization  kern.hv_support = 1
✓ ssh             /usr/bin/ssh

All checks passed. `sprout up` should work in a flake with a sprout.vms definition.
```

On Linux, `linux builder` reports the native build, `virtualization` opens
`/dev/kvm`, and `rootless shares` starts a `virtiofsd` to prove its user
namespace sandbox works:

```console
$ sprout doctor
✓ platform        x86_64-linux
✓ nix             nix (Nix) 2.34.8
✓ flakes          nix-command flakes
✓ linux builder   native local build (same-architecture x86_64-linux guest needs no remote builder)
✓ virtualization  /dev/kvm is usable (KVM API 12)
✓ rootless shares virtiofsd's namespace sandbox works (probed /nix/store/…-virtiofsd-1.14.0/bin/virtiofsd)
✓ ssh             /run/current-system/sw/bin/ssh

All checks passed. `sprout up` should work in a flake with a sprout.vms definition.
```

A configured builder is not yet a working one, so before the first boot also
run a real, trivial Linux build through it:

```console
$ sprout doctor --build
```

It adds a `linux build` line to the checks above.

## Declare a VM

In a repository with no flake yet, `sprout init` writes one:

```console
$ sprout init
wrote flake.nix declaring sprout.vms.dev

In a Git repository, stage the file so Nix can read it:
  git add flake.nix

Boot it with:
  sprout up
```

The generated configuration is below. `sprout init --vm NAME` names the
definition it declares, `dev` by default. `sprout up` then builds the flake's
only definition without being told; `sprout up --vm NAME` picks one when a
flake declares several. `systems` lists the hosts the flake builds a VM for;
keeping all three lets the same flake boot on a Mac and on a Linux machine.

```nix
# flake.nix
{
  inputs.sprout.url = "github:natsukium/sprout";
  inputs.nixpkgs.follows = "sprout/nixpkgs";
  inputs.flake-parts.follows = "sprout/flake-parts";

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      imports = [ inputs.sprout.flakeModules.default ];

      sprout.vms.dev = {
        vcpu = 4;
        mem = "8GiB";
        workspace = true; # mount the git toplevel at /workspace
      };
    };
}
```

`init` refuses to touch a flake you already have. To add sprout to one, paste
the `inputs.sprout.url` line, the `imports` entry, and the `sprout.vms.dev`
block above into it, and make sure its `systems` names your host; the rest of your flake stays as it is. If your flake does not use [flake-parts](https://flake.parts),
declare the same options through
[`lib.mkVMs`](../reference/configuration.md#without-flake-parts) instead, for
the same result. The runnable [examples](../../examples/) use the flake-parts
form in complete project flakes.

## Boot it

Nix excludes untracked files from a Git flake, so stage the generated flake
before the first boot (a commit is not required):

```console
$ git add flake.nix
$ sprout up
building and booting "main" in the background (log: …/up.log) …
VM ready. Enter it with: sprout shell
```

The instance is named after your current git branch (in a repository with
no commit yet, after its directory instead). The first boot builds
the guest, so it takes a moment; later boots reuse the build. `sprout up`
returns once the VM is ready and leaves it running; watch the console with
`sprout logs --follow`.

## Run a command and open a shell

Run a one-off command, then drop into an interactive shell:

```console
$ sprout exec -- uname -a
Linux sprout-dev 6.x.x … aarch64 GNU/Linux     # x86_64 on an x86_64 Linux host
$ sprout shell
[root@sprout-dev:/workspace]# ls     # your repo, mounted live
```

`main` is the instance name, derived from the branch; `dev` is the VM
definition selected from `sprout.vms.dev`, and the guest hostname follows that
definition. Several instances can share `sprout-dev` safely because each VM has
its own network and hostname namespace. CLI selection and routing continue to
use the instance name.

The shell logs in at `/workspace`, your working tree shared from the host, so
edits on either side are visible immediately.

## Throw it away

```console
$ exit
$ sprout delete
delete instance "main" including its persistent /var volume? [y/N] y
instance "main" deleted
```

`delete` removes this instance's state directory.

## Next

- Run one command in a throwaway VM without naming it:
  [`sprout run`](../reference/cli.md#sprout-run).
- Boot a second branch next to this one and compare the two:
  [compare branches side by side](../how-to/compare-branches.md).
- Reach a branch's web app in the browser by name:
  [reach instances by name](../how-to/route.md).
- Give the guest your GitHub or AWS credentials:
  [project host credentials](../how-to/project-credentials.md).
- Understand what "named after your branch" really means, and what happens
  when you switch branches: [instance identity](../explanation/instances.md).
