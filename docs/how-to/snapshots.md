# Snapshot and fork a volume

An instance's `/var` is the only thing that survives `sprout stop`, and it is
where the expensive state lives: a booted k3s cluster, a seeded database, a
warm build tree. `sprout snapshot` saves that volume so you can return to it, and
`sprout fork` hands it to another environment.

On a filesystem with copy-on-write clones (APFS, where macOS keeps its system
and default data volumes), both take single-digit milliseconds and no
additional disk until written, even for a 40 GiB volume; see [when it is not
instant](#when-it-is-not-instant) for the fallback on filesystems without
them.

## Save a rollback point

```bash
sprout stop
sprout snapshot create before-migration
sprout snapshot list
```

```
NAME              CREATED                    STATE  SIZE
before-migration  2026-07-31T02:09:57+09:00  clean  40.3GiB
```

The instance must be stopped, because a volume being written by a running
guest has no consistent state to capture. If stopping is too expensive (a
cluster that takes minutes to come back), `--live` snapshots it as it runs:

```bash
sprout snapshot create --live before-migration
```

The result is *crash-consistent*: it matches a power cut, so the guest's
journal replays on the next boot. This risk is why `--live` is explicit.

## Roll back

```bash
sprout stop
sprout snapshot restore before-migration
sprout start
```

Restore discards the current `/var` and prompts before it does; `--force`
skips the prompt. There is no `--live` here: swapping the disk under a
running VM corrupts both it and whatever the guest still holds in memory.

Snapshots are not automatically pruned. Delete one with `sprout snapshot delete
before-migration`; `sprout delete` on the instance takes all of them with it, and
counts them in its prompt first.

## Hand an environment to another branch

The slow part of a new branch VM is rarely the build; it is everything you do
*after* boot: deploying, seeding, waiting for things to converge. Where that
work produces state a second branch can simply share, fork the volume instead
of repeating it (read [what a fork does not
change](#what-a-fork-does-not-change) first; some of it cannot be shared):

```bash
git worktree add .worktree/feat-login -b feat-login
cd .worktree/feat-login
sprout stop -i main
sprout fork -i main
sprout start
```

`-i` selects the source. The current directory identifies the destination
(here the `feat-login` branch), or a positional operand names it explicitly
(`sprout fork -i main experiment`).

The source must be stopped unless you pass `--live`. A live fork uses an
atomic copy-on-write clone, but its volume is crash-consistent because writes
held in guest memory are absent. It is refused on a filesystem without
copy-on-write clones.

The destination must not already exist. `fork` never replaces an environment
and has no `--force`: the state it would overwrite is exactly the persistent
volume someone forked in order to keep.

Only the volume and the recorded build come across. Credentials, shared and
project caches, and ssh material are re-projected on the fork's first boot
like any other instance's, so nothing is stale. Instance-scoped caches sit on
`/var`, so the fork inherits a copy and starts warm. A later `sprout up`
rebuilds against this worktree's flake if the definition has since diverged.

### What a fork does not change

A fork copies `/var` verbatim, which means it also copies whatever identity
the guest's stateful services wrote into it the first time they were set up:
cluster hostnames, issued certificates, OIDC client IDs, database rows keyed to
a domain. The fork is a second copy of *that* environment, not a fresh one
that happens to start warm.

A service that pins identity at bootstrap retains it in the fork. For example,
a Kubernetes stack keeps its original ingress domains and identity-provider
configuration, which may reject a later domain change. Such an environment
must establish its branch domain when first created; fork it only when both
copies should retain that identity.

Volumes whose contents are not identity-bearing (a warm build tree, a package
cache, a database seeded with data that carries no hostname) have no such
caveat and fork cleanly.

## What the reported sizes mean

`list`, `inspect`, and `snapshot list` count allocated blocks per file, and a
fork shares its blocks with the source. Right after forking, two 40 GiB
images together occupy the space of one, while each still reports 40 GiB. They
diverge, and start costing real disk, only as each side is written.

The reported sizes are therefore not additive. Check total filesystem usage
with:

```bash
du -sh "${XDG_STATE_HOME:-$HOME/.local/state}/sprout"
```

## When it is not instant

Copy-on-write clones are a filesystem feature, and a per-volume one: macOS
formats its own volumes as APFS, but a state directory placed on an external
or network volume with another filesystem has no clones, and neither does a
clone that would cross volume boundaries. On Linux they need btrfs, XFS,
bcachefs, or OpenZFS 2.2+; ext4 has no equivalent. Without them, sprout falls
back to a hole-skipping copy that is correct but takes real time and real
disk. It tells you which path it took:

```
snapshot "before-migration" of instance "main" created (full copy: this filesystem has no copy-on-write clones)
```

`--live` is refused on that path, since a copy that takes minutes cannot be
atomic against a guest writing to the image underneath it.
