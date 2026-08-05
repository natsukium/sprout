# Getting started

Boot your first microVM, run a command inside it, open a shell, and delete the
instance.

You need:

- macOS on Apple Silicon, and Nix with flakes enabled.
- An `aarch64-linux` builder Nix can reach. The guest is a Linux NixOS
  closure, which a Mac cannot build natively; nixpkgs'
  [`darwin.linux-builder`](https://nixos.org/manual/nixpkgs/stable/#sec-darwin-builder)
  (`nix run nixpkgs#darwin.linux-builder`, no nix-darwin required) is the
  quickest answer, and nix-darwin's `nix.linux-builder` option runs the same
  builder as a managed service. [The examples' build
  notes](../../examples/README.md#building-on-apple-silicon) list the
  alternatives and the project binary cache. Without a builder, the first
  `sprout up` fails inside `nix build`.

## Get sprout and check the prerequisites

Get `sprout` itself on your PATH: ad hoc with `nix shell github:natsukium/sprout`,
or durably by adding the `sprout` package to your dev shell. `sprout doctor`
verifies the prerequisites above and prints the fix for anything missing:

```console
$ sprout doctor
✓ platform        macOS arm64
✓ nix             nix (Nix) 2.28.3
✓ flakes          nix-command flakes
✓ linux builder   builders = @/etc/nix/machines
✓ virtualization  kern.hv_support = 1
✓ ssh             /usr/bin/ssh

All checks passed. `sprout up` should work in a flake with a sprout.vms definition.
```

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
flake declares several.

```nix
# flake.nix
{
  inputs.sprout.url = "github:natsukium/sprout";
  inputs.nixpkgs.follows = "sprout/nixpkgs";
  inputs.flake-parts.follows = "sprout/flake-parts";

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [ "aarch64-darwin" ];
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
block above into it; the rest of your flake stays as it is. If your flake does not use [flake-parts](https://flake.parts),
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

The instance is named after your current git branch. The first boot builds
the guest, so it takes a moment; later boots reuse the build. `sprout up`
returns once the VM is ready and leaves it running; watch the console with
`sprout logs --follow`.

## Run a command and open a shell

Run a one-off command, then drop into an interactive shell:

```console
$ sprout exec -- uname -a
Linux sprout-dev 6.x.x … aarch64 GNU/Linux
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
