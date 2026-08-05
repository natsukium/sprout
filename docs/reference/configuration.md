# Configuration reference

All options below are the same whether you declare a VM through the
`sprout.vms.<name>` flake-parts module or through
[`lib.mkVMs`](#without-flake-parts) in a plain flake.

## Options

| Option | Default | Description |
| --- | --- | --- |
| `vcpu` | `4` | Virtual CPUs. |
| `mem` | `8192` | Memory: an integer MiB count, or a string with a unit (`"8GiB"`, `"512MiB"`). |
| `diskSize` | `102400` | Persistent `/var` volume size (sparse); integer MiB or a unit string. |
| `workspace` | `true` | Mount the git toplevel at `/workspace` (virtiofs). |
| `writableStore` | `false` | Overlay a writable `/nix/store` over the host share, so `nix build` works inside the guest. The overlay lives on the persistent `/var` volume, so big closures don't eat guest RAM and paths built once survive `stop`/`up`. |
| `hostLoopback` | `false` | Let the guest reach the host's loopback at `192.168.127.254`. Opt-in because it exposes every `127.0.0.1` listener on the host to the guest; see [architecture](../explanation/architecture.md#what-the-guest-can-reach). |
| `dns.wildcardDomains` | `[]` | Domains whose every subdomain resolves to `127.0.0.1` inside the guest; other queries resolve as they otherwise would. The embedded stack's resolver answers them at the gateway, so a container that inherits the guest's `/etc/resolv.conf` resolves them too. `[ "sprout.localhost" ]` makes per-branch router domains resolve in-guest, with no per-branch config. |
| `credentials` | `{}` | Host credentials projected into the guest, opt-in per name. |
| `caches` | `{}` | Named build caches shared across instances. |
| `modules` | `[]` | NixOS modules merged into the guest. |
| `idle.action` | `"stop"` | `"stop"` auto-stops after `idle.after` of no activity; `"none"` disables it. |
| `idle.after` | `"2h"` | Idle period before `idle.action` fires, as a Go duration. |

Activity means an open SSH session or a routed connection bridged through
the router; the idle clock restarts when the last one closes. A plain
`sprout forward` does not count, so a forwarded port cannot pin an
otherwise-idle VM in memory. The router tracks connections, not browser
tabs: a page holding a connection open (a dev server's reload socket) keeps
the instance alive, while a tab whose keep-alive connection has closed does
not, and the next request wakes the instance again.

## Workspace

Sessions default into the mount: an interactive `sprout shell` (and the console)
logs in at `/workspace` via the guest's login init, and `sprout exec -- <cmd>` /
`sprout run` prepend the same `cd` to the remote command (added host-side),
since non-interactive shells never read login init. A VM declared with
`workspace = false` starts in `$HOME`.

`workspace` also shares the repository's git common directory, so a linked
`git worktree` does not leave its `.git` file pointing outside the mount.
`up` derives the workspace form from its current directory. This has two
consequences:

- Every instance of one clone shares one object store and one set of refs, the
  same way the host's worktrees do: a commit or `fetch` in any of them is
  immediately visible to all the others and to the host.
- The guest can see the *registrations* of the clone's other worktrees, which do
  not resolve inside it. `git gc` is kept from pruning them (the guest sets
  `gc.worktreePruneExpire = never`), but an explicit `git worktree prune` inside
  a guest still deletes host-side registrations. A guest that sets its own
  `/etc/gitconfig` replaces that guard.

A **submodule** checkout has the same outward shape and is not covered: its
worktree is found through `core.worktree`, not through the worktree
registration this relies on. `up` still warns there, as it does for a bundle
that does not declare the git share; give those guests a full clone per branch.

The share makes the repository readable but does not install `git`. Add it
through `environment.systemPackages` under `modules` when needed. The share
lands at `/run/sprout-git`; without git, inspect its host-path symlinks with
`ls -l`.

## Without flake-parts

`sprout.vms.<name>` is a flake-parts module, but adopting sprout must not mean
restructuring a flake that does not use flake-parts. `lib.mkVMs` takes the
same options and returns the `sproutConfigurations` attrset directly:

```nix
{
  inputs.sprout.url = "github:natsukium/sprout";

  outputs = { self, sprout }: {
    sproutConfigurations = sprout.lib.mkVMs {
      pkgs = sprout.inputs.nixpkgs.legacyPackages.aarch64-darwin;
      vms.dev = {
        vcpu = 2;
        mem = "2GiB";
        modules = [ ./guest.nix ];
      };
    };
  };
}
```

`pkgs` is the **host** (`aarch64-darwin`) package set: the runner is a host
artifact, while the guest closure is built from sprout's own pinned nixpkgs. The result
is byte-for-byte the bundle the flake-parts module produces for the same
options, so every command, option, and guest feature behaves identically.

`sprout` discovers definitions by the attr names of the `sproutConfigurations`
flake output, a dedicated output rather than `packages` so dev VMs never
clutter `nix flake show` or a project's real package set. `lib.mkVM` returns
a single bundle (taking `name` and the VM options inline) if you need to
assemble that attrset yourself.

## Readiness

"Ready" (what `sprout up` returns on, what `list` reports, when the idle clock
starts, and when the router stops serving its waking page) means the guest
answers SSH **and** has reached its `sprout-ready.target`. The target is plain
systemd: hook any unit that must complete before the instance counts as
usable (a cluster bootstrap, a data migration):

```nix
systemd.services.my-bootstrap = {
  # …
  requiredBy = [ "sprout-ready.target" ];
  before = [ "sprout-ready.target" ];
};
```

With nothing hooked, the target is reached at boot and readiness is
effectively SSH-up. The readiness marker is part of every bundle, so the
wait budget before `up` warns is the same 10 minutes whether or not
anything is hooked; an unhooked guest simply spends seconds of it. The `k3s` module hooks the target itself, so a guest
importing it waits for a schedulable node without writing the unit above. If a hooked unit fails, the instance is never
announced ready; the timeout warning plus `sprout logs` is the signal to look
inside. Under the hood the guest's `sprout-ready-notify` unit touches
`/run/sprout/ready` on the data share; the host only ever stats its side of
that directory.

## Credentials

Every credential resolves to one of three strategies. A built-in fills in a
sensible default per field; a custom entry chooses explicitly. To enable
one, see [project host credentials](../how-to/project-credentials.md).

| Strategy | What it does | When it fits |
| --- | --- | --- |
| `mount` | virtiofs mount of a host path, `readOnly` default `true` | File-based credentials where host-side refresh should be live in the guest. `readOnly = false` also lets the guest write back, at the cost of guest processes being able to modify the host credential. |
| `materialize` | run a host command at boot time, write its stdout into per-instance state | Secrets with no file on the host, such as a token returned by a host credential helper. Refreshed on each boot; an `up` that finds the instance already running leaves the projection as it is. |
| `socket` | proxy a host unix socket into the guest over the network stack | Agent protocols (`ssh-agent`); the secret never enters the VM. |

### Built-ins

| Credential | Strategy | Notes |
| --- | --- | --- |
| `gh` | materialize | Keychain token via `gh auth token`, rendered into the guest's `hosts.yml`. |
| `aws` | mount, rw | `~/.aws` mounted live so `aws sso login` on either side works. Set `readOnly = true` to lock it down. |
| `aws-config` | materialize | Effective AWS config (including `$AWS_CONFIG_FILE`) rendered at boot and selected with `AWS_CONFIG_FILE`. |
| `ssh-agent` | socket | Host `SSH_AUTH_SOCK` forwarded; keys never enter the VM. |

A custom entry: `credentials.<name> = { enable = true; strategy =
"mount"|"materialize"|"socket"; source; target; readOnly ? true; }`, plus the
strategy-specific fields: `exec` (the host command a `materialize` runs),
`guestPath` (where the materialized file or forwarded socket appears),
`guestPort` (the gateway port of a `socket` forward), and `guestEnv`
(environment variables set in the guest, e.g. `SSH_AUTH_SOCK`).

Secrets are projected per-instance and per-boot, never baked into an image
or the Nix store, and mounts are read-only unless you opt out.

Built-ins are modules under `nix/modules/credential/<name>/`. Each declares
per-field defaults behind `enable`, and all fields remain overridable.

## Caches

Named read-write directories reused across instances; opt in per name,
built-ins included. `scope` decides both how far a cache reaches and what
backs it — a host virtiofs share, or the guest's own volume. To declare and
manage them, see [share build caches](../how-to/share-caches.md).

| Property | What it controls |
| --- | --- |
| `enable` | Opt this cache in (`false` by default, built-ins included). |
| `guestPath` | Where the cache mounts inside the guest. |
| `scope` | `"project"` (default) reuses one host tree across every instance of the same clone; `"shared"` widens that to every project on the host; `"instance"` drops the host share and backs the cache with the guest's own `/var`, removed by `sprout delete`. |
| `guestEnv` | Environment variables set in the guest, so the tool using the cache is pointed at `guestPath` where the cache is declared (e.g. `SCCACHE_DIR`). |
| `guestModule` | NixOS module merged into the guest while the cache is enabled: how a built-in ships the tool its cache feeds. Credentials accept the same field. |

### Built-ins

Like credentials, built-in caches live under `nix/modules/cache/<name>/`
and stay inert until enabled:

| Cache | Defaults | Notes |
| --- | --- | --- |
| `sccache` | shared, `/root/.cache/sccache` | Also installs sccache in the guest and sets `RUSTC_WRAPPER`/`SCCACHE_DIR`, following an overridden `guestPath`. `maxSize = "20G"` caps the cache on disk (`SCCACHE_CACHE_SIZE`). |
| `cargo-registry` | instance, `/root/.cargo/registry` | Instance scope keeps the registry on the guest's native filesystem, where cargo's file locks behave and its many small extracted crate sources are not read over virtiofs. |
| `pnpm-store` | shared, `/root/.local/share/pnpm/store` | Pins the store through `pnpm_config_store_dir` (pnpm 11) and `npm_config_store_dir` (pnpm 10 and earlier), following an overridden `guestPath`; across virtiofs mounts pnpm degrades to copy mode, trading disk dedup for download reuse. |

Built-ins may add options such as `sccache.maxSize`; every entry has the common
properties above.

A shared cache lives at `~/.cache/sprout/<arch>/<name>`, a project cache at
`~/.cache/sprout/<arch>/.projects/<repo>-<hash>/<name>`, where the hash is
taken over the clone's git-common-dir and the label is the repository
directory it sits in. The `~/.cache/sprout` root follows `XDG_CACHE_HOME`
when the variable is set. The host's own `~/.cargo` and `~/Library/Caches` are
never used: host artifacts are darwin, guest artifacts are linux, and mixing
them confuses tooling. Caches are keyed by guest arch so a future x86_64
(Rosetta) guest gets its own tree.

An instance cache has no host directory at all: it is a directory on the
guest's `/var` volume, bind-mounted to `guestPath` before sshd accepts the
first command. It therefore counts against `diskSize`, is captured by
`sprout snapshot`, is copied by `sprout fork`, and is invisible to
`sprout cache list` and `cache delete`.

Content-addressed stores (sccache, the pnpm store) tolerate concurrent
writers by design, which is what makes their `shared` scope safe.

## Guest profiles

Reusable NixOS modules for `modules`, carrying the sprout-specific plumbing a
stack needs so project configs hold only project choices:

| Module | What it provides |
| --- | --- |
| `inputs.sprout.nixosModules.k3s` | Single-node k3s: the kernel modules and sysctls pod networking needs under the microvm kernel, `kubectl` + `KUBECONFIG`, a kubeconfig left dialable and self-contained (its CA embedded, so copying it to the host works, and its server pinned to loopback, which repairs the `0.0.0.0` a guest setting `--bind-address` would otherwise get), and readiness held back until the node is schedulable, so `up` returns to a cluster kubectl can act on. `sprout.k3s.readyTarget` names the target to hold (null waits for nothing) and `sprout.k3s.kubeconfig` where k3s writes the file. Project flags (`services.k3s.extraFlags`, `disable`, registries) stay yours. |
| `inputs.sprout.nixosModules.podman` | Rootful Podman with network definitions under persistent `/var`. This keeps containers attached to runtime-created networks across `sprout stop` / `sprout up`; the rootful Podman default under `/etc` would be rebuilt at boot. Rootless Podman has separate per-user storage and is outside this profile. |

See [examples/k3s](../../examples/k3s/) and [examples/podman](../../examples/podman/) for the profiles in use.
