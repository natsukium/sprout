# Migrate to system-qualified outputs

Releases up to 0.1.3 supported only Apple Silicon macOS, and a flake exposed
its VMs as `sproutConfigurations.<name>`. Since Linux hosts were added, a VM
is built per host system, so the output is
`sproutConfigurations.<system>.<name>`, and the bundle's `manifest.json` is
schema version 2, which names the host, guest, and backend it was built for.
This guide moves an existing project and its instances across.

## What changed

| | Before | Now |
| --- | --- | --- |
| Bundle output | `sproutConfigurations.<name>` | `sproutConfigurations.<system>.<name>` |
| Guest system output | `nixosConfigurations.sprout-<name>` | `nixosConfigurations.sprout-<system>-<name>` |
| Hosts the flake-parts module builds for | `aarch64-darwin`, if listed in `systems` | every supported host listed in `systems`: `aarch64-darwin`, `aarch64-linux`, `x86_64-linux` |
| `lib.mkVMs` result | the whole output | one host's slice, which you nest under that host's system |
| `manifest.json` | version 1: vfkit implied | version 2: `host.system`, `guest.system`, and a `backend` (kind, network transport, control protocol, console mode, sidecars) |
| `instance.json` | version 1, no platform | version 2, recording host, guest, and backend |
| `snapshot.json` | no version field, no platform | version 2, recording host, guest, and backend |
| VM option | — | [`backend`](../reference/configuration.md#options), default `"auto"` |

`<system>` is the system of the host that runs `sprout`, not the guest's:
the Apple Silicon bundle is `sproutConfigurations.aarch64-darwin.<name>`,
even though its guest is `aarch64-linux`.

## Upgrade the binary and the flake together

The `sprout` binary reads the output shape and the manifest version, and the
flake's `sprout` input produces them, so move both in one step. A new binary
pointed at an old flake stops before building:

```text
sprout: . exposes VMs as sproutConfigurations.<name> (dev), but this sprout reads sproutConfigurations.<system>.<name>; update the flake's sprout input (nix flake update sprout), or nest a lib.mkVMs result under the host: sproutConfigurations.aarch64-darwin = sprout.lib.mkVMs { ... }
```

An old binary rejects a version-2 manifest instead. If `sprout` comes from
your dev shell through the same input, updating the input upgrades both.

## A flake using the flake-parts module

Update the input:

```console
$ nix flake update sprout
```

The module now builds one bundle for every supported host in `systems`, so
list each host the project's developers use. The `sprout init` template lists
all three:

```nix
systems = [
  "aarch64-darwin"
  "aarch64-linux"
  "x86_64-linux"
];
```

A host missing from `systems` gets no bundle, and `sprout up` there says which
system to add:

```text
sprout: . defines VMs but not for x86_64-linux (it targets aarch64-darwin); add "x86_64-linux" to systems
```

A `sprout.vms.<name>` definition itself needs no change. It is evaluated once
and built for each host, so anything in it that names an architecture has to
follow the host instead; a `withSystem "aarch64-linux"` that picks guest
packages is the common case, and [reuse the
devShell](reuse-the-devshell.md) shows the host-independent form.

## A flake using `lib.mkVMs`

`lib.mkVMs` returns the VMs for the one host whose `pkgs` it receives. Nest
that result under the host's system, once per host:

```nix
{
  inputs.sprout.url = "github:natsukium/sprout";

  outputs = { self, sprout }: {
    sproutConfigurations = sprout.inputs.nixpkgs.lib.genAttrs
      [ "aarch64-darwin" "aarch64-linux" "x86_64-linux" ]
      (system: sprout.lib.mkVMs {
        pkgs = sprout.inputs.nixpkgs.legacyPackages.${system};
        vms.dev.modules = [ ./guest.nix ];
      });
  };
}
```

A project that only ever runs on Macs can write the single slice,
`sproutConfigurations.aarch64-darwin = sprout.lib.mkVMs { … };`.

## Scripts and prebuilt bundles

Anything that names the output by path needs the system inserted. That covers
a CI job that prebuilds the bundle, a `nix build` you run before `sprout up
--bundle`, and tooling that inspects the guest:

```console
$ nix build .#sproutConfigurations.aarch64-darwin.dev        # was .#sproutConfigurations.dev
$ nix eval .#nixosConfigurations.sprout-aarch64-darwin-dev.config.networking.hostName
```

A bundle built before the change carries a version-1 manifest. The new binary
still boots it on `aarch64-darwin`, as the vfkit bundle it always was. On a
Linux host it refuses one and asks for a rebuild with a flake that emits
version 2, which the steps above produce.

The nix-darwin module builds its bundles from the `sprout` input it was
imported from, so updating that input and running `darwin-rebuild switch` is
the whole migration for supervised instances.

## Existing instances and snapshots

On `aarch64-darwin` nothing needs to be deleted. An instance or snapshot
record written before the change can only have come from vfkit on that host,
so the new binary reads it as `aarch64-darwin` / `aarch64-linux` / `vfkit`, and
rewrites an instance record as version 2 on its next write. `sprout start`
keeps booting the build an instance recorded, and the next `sprout up`
rebuilds it from the updated flake.

That rewrite is one-way: a 0.1.x binary refuses a version-2 record and asks
for a delete, so do not go back to an older `sprout` once a new one has
touched an instance.

On Linux there are no old instances to migrate. A record copied from a Mac
lists and deletes but is refused for reuse, because its disk was made for a
different host and backend; see [reusing a
disk](../reference/instance-state.md#reusing-a-disk).
