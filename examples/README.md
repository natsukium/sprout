# Examples

Start with [`minimal/`](minimal/) to verify your setup before the container
examples add image pulls and builds.

The other examples run the same Postgres-backed todo app. `sprout up` creates
the guest; start the stack with its container runtime:

- [`podman/`](podman/) — a plain [`compose.yaml`](podman/compose.yaml) run
  with podman.
- [`k3s/`](k3s/) — plain [Kubernetes manifests](k3s/manifests/stack.yaml) on
  a single-node k3s cluster, deployed with skaffold.

[`app/`](app/) contains a small Go todo app and its
[`Containerfile`](app/Containerfile). The compose file and Kubernetes manifests
use standard container tooling; Nix configures the guest, not the stack.

## Building on Apple Silicon

A sprout guest is an `aarch64-linux` NixOS closure, so a Mac cannot build it
natively. You need an `aarch64-linux` builder Nix can reach. The quickest
path, with no extra tooling, is nixpkgs'
[`darwin.linux-builder`](https://nixos.org/manual/nixpkgs/stable/#sec-darwin-builder):
run `nix run nixpkgs#darwin.linux-builder` and point the `builders` setting
at it, as the linked section shows. nix-darwin runs the same builder as a
managed service through its `nix.linux-builder` option. A remote Linux
machine in `/etc/nix/machines`, or a Linux CI runner (for example a Linux
GitHub Actions runner) building the VM for you, also works.

CI pushes every example's guest closure and the `sprout` CLI to the project
binary cache on each push to `main`, so a checkout of `main` fetches the heavy
closures (k3s pulls in a lot) instead of building them. Add the cache to your
Nix settings:

```
extra-substituters = https://nix-cache.natsukium.com
extra-trusted-public-keys = niks3-1:SoIFTPtiPoCW3/OzUkIBKlLG5znMZfbihlr11XAOles=
```

On a multi-user Nix install these lines belong in the system configuration
(`/etc/nix/nix.conf`, or nix-darwin's `nix.settings`): Nix ignores
substituters added by a user it does not trust. A cache hit requires the same
evaluation CI ran, so it covers the examples as committed; once you change an
example or its inputs, the builder above takes over.

## The stack

```
browser ──sprout forward──▶ guest ──▶ web (todo app) ──▶ db (Postgres)
```

The app retries its database connection on startup until Postgres answers,
so nothing depends on start order: the stack comes up whether the database or
the app wins the race.

## Run one

```console
$ cd examples/podman     # or examples/k3s
$ sprout up
$ sprout shell              # then start the stack — see the example's README
```

The podman example runs `podman-compose up`; the k3s example runs `skaffold
run` (or `skaffold dev` for a live rebuild loop). Once the stack is up, forward
the port it publishes — `sprout forward 8080` for podman, `sprout forward
--bind 0.0.0.0 80` for k3s — and add a few todos. They persist across `sprout
stop` / `sprout up`, because the database volume lives on the guest's
persistent `/var`. Delete the instance with `sprout delete`.

The first run is slower: the runtime pulls the Postgres and build base images
and builds the app image. Later runs reuse them from `/var`.

Starting the stack by hand keeps the guest config app-agnostic and matches the
real dev loop. If you want `sprout up` to bring the stack up unattended (for a CI
runner, say), each README shows the optional systemd unit to add.

Each example pins sprout with `inputs.sprout.url = "path:../.."` so it works from a
checkout of this repository. The examples also live *inside* the repo, so the
guest builds from `/workspace/examples/app` (the mounted git toplevel). In
your own project the app would sit at the repo root and the paths would be
`/workspace`.

## Run both at once

An instance belongs to a (repository, branch) pair, so from one branch the two
directories select the same instance and `sprout up` replaces its VM definition
in place. Either finish an example through its `sprout delete` step before
starting the next, or give the second one a branch of its own and run them side
by side:

```console
$ cd "$(git rev-parse --show-toplevel)"
$ git worktree add ../sprout-k3s -b try-k3s
$ cd ../sprout-k3s/examples/k3s
$ sprout up
```

The podman instance keeps running on its branch, with its own `/var`,
containers, and guest network. Their host ports differ, so both forwards can
run at the same time and you can put the compose stack and the cluster in two
browser windows. [Comparing two candidate
implementations](../docs/how-to/compare-branches.md) is the same machinery, and
adds one router that serves every instance by name, `sprout exec` for what a
browser does not show, and `sprout fork` to start both sides from the same
data. Two VMs also cost two `mem` allocations; k3s alone asks for 6 GiB.

## Why two runtimes

A full-VM guest can run nested container runtimes, and the two examples take
the same app to opposite ends of the loop. Podman is the everyday one: a
compose file, a fast start, few resources, the app published straight out of
the guest on 8080. The k3s example pays for a control plane and buys a
production-shaped target — Deployment, Service, PersistentVolumeClaim, an image
registry to push to, and an ingress on the conventional port 80 — so a manifest
or a rollout can be checked here rather than in a real cluster. Both keep
application files independent of sprout.
