# Share build caches across instances

Declare a build cache once and every instance that opts in mounts it, so a
cold checkout reuses artifacts a sibling already downloaded or compiled.

For the sharing and arch-keying semantics, see the [caches
reference](../reference/configuration.md#caches).

## Enable a built-in

```nix
sprout.vms.dev.caches = {
  sccache.enable = true;
  cargo-registry.enable = true;
  pnpm-store.enable = true;
};
```

`sccache.enable = true` mounts the cache, installs sccache, and sets
`RUSTC_WRAPPER`/`SCCACHE_DIR`. Overriding a module default such as `guestPath`
keeps the other defaults. Built-ins may add tool-specific options;
`sccache.maxSize = "20G"` sets `SCCACHE_CACHE_SIZE`.

## Declare a custom cache

```nix
sprout.vms.dev.caches.bazel-disk = {
  enable = true;
  guestPath = "/root/.cache/bazel-disk";
};
```

A host-backed cache (`project` or `shared` scope) becomes a read-write
virtiofs mount at its `guestPath`, backed by a host directory and shared by
every instance the scope covers; an `instance`-scoped cache is a bind mount
from the guest's own persistent `/var`, private to that instance.

## Choose a scope

`scope` decides how far a cache reaches:

| Scope | Reaches | Host directory |
| --- | --- | --- |
| `project` (default) | every instance of the same clone, so branches share | `~/.cache/sprout/<arch>/.projects/<repo>-<hash>/<name>` |
| `shared` | every project on the host | `~/.cache/sprout/<arch>/<name>` |
| `instance` | one instance, removed by `sprout delete` | the guest's own `/var`, no host share |

The `~/.cache/sprout` root follows `XDG_CACHE_HOME` when the variable is set.

The default stops at the clone because a shared tree is writable by every
project on the host: a semi-trusted workload in one guest can poison
artifacts another project later builds against. Widen to `shared` when the
cache addresses its contents by their own inputs, so another project's entry
is a valid entry here: `sccache` keys on compiler inputs and `pnpm-store` on
package content, and both ship as `shared`. `cargo-registry` ships as
`instance` instead, because cargo's file locks and its many small extracted
sources want the guest's own filesystem rather than a host share (see [keep a
cache per-instance](#keep-a-cache-per-instance)).

```nix
sprout.vms.dev.caches.bazel-disk.scope = "shared";
```

Most tools only use a cache they are pointed at. `guestEnv` sets the guest
environment variables next to the mount they describe, and `guestModule`
merges a NixOS module into the guest while the cache is enabled, so the
tool, its cache, and its pointers travel together:

```nix
sprout.vms.dev.caches.bazel-disk = {
  enable = true;
  guestPath = "/root/.cache/bazel-disk";
  guestEnv.BAZEL_DISK_CACHE = "/root/.cache/bazel-disk";
  guestModule = { pkgs, ... }: { environment.systemPackages = [ pkgs.bazelisk ]; };
};
```

## Ship a reusable cache module

A one-off entry belongs inline as above. Once the same cache (defaults,
guest tooling, knobs) is repeated across projects, package it the way
sprout's own built-ins are packaged: a VM definition is itself a module, so
a flake can export a cache module and any project imports it into its
definition. Extend `lib.cacheEntryModule` (`credentialEntryModule` for
credentials) to declare the entry, including options of your own that plain
entries don't have:

```nix
# In the publishing flake: sproutModules.bazel-disk
{ lib, ... }:
{
  options.caches.bazel-disk = lib.mkOption {
    type = lib.types.submodule [
      inputs.sprout.lib.cacheEntryModule
      (
        { config, ... }:
        {
          options.repositoryCache = lib.mkOption {
            type = lib.types.bool;
            default = false;
            description = "Also point Bazel's repository cache at the share.";
          };
          config = {
            guestPath = lib.mkDefault "/root/.cache/bazel-disk";
            guestEnv = lib.mkIf config.repositoryCache { BAZEL_REPO_CACHE = config.guestPath; };
            guestModule = lib.mkDefault ({ pkgs, ... }: { environment.systemPackages = [ pkgs.bazelisk ]; });
          };
        }
      )
    ];
    default = { };
  };
}
```

```nix
# In a consuming project:
sprout.vms.dev = {
  imports = [ inputs.bazel-tools.sproutModules.bazel-disk ];
  caches.bazel-disk = {
    enable = true;
    repositoryCache = true;
  };
};
```

`nix/modules/cache/sccache` uses this same module interface.

## Inspect and reclaim

Caches are not tied to any running instance, so `list` and `delete` work with no
VM up:

```console
$ sprout cache list
NAME            ARCH           PROJECT        SIZE     LAST USED
bazel-disk      aarch64-linux  sprout-9f2c1a  820MiB   2026-07-11T…
sccache         aarch64-linux  -              1.2GiB   2026-07-11T…
$ sprout cache delete sccache
removed cache "sccache" (aarch64-linux)
```

`delete` spans every arch tree and every project holding the name, so one
call clears a cache the whole host over. Removing a cache makes the next
build cold; no instance locks it.

## Keep a cache per-instance

`scope = "instance"` opts out of sharing entirely. The cache is then backed
by the guest's own persistent `/var` volume and bind-mounted to `guestPath`,
so it never becomes a host share:

```nix
sprout.vms.dev.caches.cargo-target = {
  enable = true;
  guestPath = "/workspace/target";
  scope = "instance";
  guestEnv.CARGO_TARGET_DIR = "/workspace/target";
};
```

This is the scope to reach for when the cache is write-heavy or holds many
small files: it runs on the guest's native filesystem rather than virtiofs,
and file locks behave the way the tool expects. Two consequences follow from
living on `/var`:

- it counts against `diskSize`, and it shares that volume with the writable
  Nix store overlay and service state, so cap anything unbounded (`sccache`
  has `maxSize`);
- `sprout snapshot` captures it and `sprout fork` copies it, which starts a
  fork warm but also rolls the cache back with a `snapshot restore`.

`sprout cache list` and `cache delete` do not cover instance caches, because
they never reach the host tree. `sprout delete` removes them with the volume.
