# Instance state

## On-disk layout

```
~/.local/state/sprout/
├── id_ed25519                    # shared client key for every instance
└── instances/<id>/
    ├── instance.json             # version, repository, branch, definition, bundle path
    ├── var.img                   # persistent /var (sparse)
    ├── var.img.restoring         # transient staging file during `snapshot restore`
    ├── run.sh                    # per-instance runner (placeholders substituted)
    ├── control.sock              # daemon control socket (present while running)
    ├── net.sock                  # vfkit datagram socket into the network stack
    ├── vfkit-rest.sock           # vfkit REST endpoint, used by graceful stop
    ├── daemon.lock               # held by the running daemon (see below)
    ├── up.log                    # stdout of the last detached `up`/`start`: build output and boot chatter
    ├── runner.log                # vfkit runner output (`sprout logs`)
    ├── console.log               # guest serial console (`sprout logs`)
    ├── data/ssh/                 # authorized_keys, projected at boot
    ├── data/instance.env         # identity env file; guest path /run/sprout/instance.env (see below)
    ├── data/credentials/<name>   # materialized secrets; removed on daemon exit (after a SIGKILL or host crash, swept by the next `stop` or boot)
    ├── known_hosts               # per-instance host key trust
    └── snapshots/<name>/
        ├── var.img               # copy-on-write clone of /var
        └── snapshot.json         # when it was taken, and against which build
```

A daemon holds `daemon.lock` for its lifetime. The kernel releases the lock on
any process exit, so a boot that acquires it can clean up a VM process left by
a crashed daemon.

`<id>` is the hash `sprout list` shows in its ID column: a hex digest of the
repository and branch (see [instance
identity](../explanation/instances.md)), so it is filesystem-safe whatever
the branch name contains. The state root follows `XDG_STATE_HOME` when set,
otherwise `~/.local/state`.

## Socket paths

Unix socket addresses are capped near 104 bytes (`sockaddr_un.sun_path`),
while the instance directory's depth follows the state root, which can be
arbitrarily deep — a self-hosted CI runner's home is enough to overflow it.
Socket system calls (bind and dial, in sprout and in vfkit) therefore address
the sockets through `/tmp/sprout-<uid>/<id>`, a symlink to the instance
directory, whose length does not depend on the state root.

The socket *files* stay in the instance directory: the layout above is
complete, and deleting the instance directory removes them. The symlink is
disposable — every bind and dial recreates it, so a `/tmp` cleaner removing it
does not cut off a running instance. `sprout delete` and `sprout prune` remove
it along with the instance; a leftover link dangles harmlessly.

`/tmp/sprout-<uid>` is created `0700` and is only accepted if it is a real
directory owned by the invoking user, so another user pre-creating the path
cannot redirect where sockets are bound. If sprout reports the directory as
someone else's, remove it and retry.

`instance.json` is schema version 1. sprout rejects an instance record with an
unsupported version before touching its runner or volume. There is no
in-place migration in the 0.1 release; keep a copy of important guest data
before deleting a record and recreating it with `sprout up`.

Shared and project build caches live under `~/.cache/sprout/` (or
`$XDG_CACHE_HOME/sprout/`); they outlive any one instance and are not
instance state. See the [caches
reference](configuration.md#caches) for the per-scope layout.

## Instance identity in the guest

Each boot writes `/run/sprout/instance.env` into the guest (via the per-instance
data share):

```sh
SPROUT_INSTANCE_ID='9f2c…'
SPROUT_INSTANCE_NAME='feat/login'
SPROUT_INSTANCE_LABEL='feat-login'
SPROUT_DEFINITION='dev'
```

This is the channel for values that vary per instance while the bundle does
not: key runtime config on it (a per-branch base domain, a cache label)
instead of rebuilding the guest per branch. Login shells (interactive
`sprout shell`, the console) have the variables exported already; a systemd unit
reads the same file with `EnvironmentFile = "-/run/sprout/instance.env"`; a
non-interactive `sprout exec -- <cmd>` does not source login init, so scripts
that need it source the file themselves. `SPROUT_GUEST=1` is separate,
build-time, and always set: it answers "am I in a sprout guest?" without
depending on this boot-time file.

`SPROUT_INSTANCE_NAME` is the raw name, which for a branch may hold characters
no hostname can (`feat/login`). `SPROUT_INSTANCE_LABEL` is the hostname label
`sprout route` answers to for that instance: the sanitized name, or the
instance ID when more than one instance answers to that label (see [reach
instances by name](../how-to/route.md#when-two-instances-share-a-name)). A
guest that builds its own external URLs reads the label. It is written per
boot, so a running instance keeps the label its guest was given until it is
next booted, even after a sibling answering to the same label appears.

## Lifecycle

The flake output is immutable and content-addressed. The instance directory is
disposable; `sprout delete` removes it.

`sprout up` after a definition change rebuilds and reboots into the new system
by comparing the freshly built store path against the one recorded in
`instance.json`; the persistent `/var` volume survives the reboot. `sprout
stop` is a full poweroff that keeps `/var`; the next `up` is a fresh boot
against it.

State lives under `~/.local/state`, not `/tmp`, so it survives host
reboots. Disk pressure is handled by `sprout list` with `sprout delete` and `sprout
prune`, and by `idle.action`, not by the OS deleting a volume out from
under a database.

## Snapshots and forks

`sprout snapshot` and `sprout fork` copy `var.img` with a copy-on-write clone:
`clonefile(2)` on APFS, the `FICLONE` ioctl on Linux filesystems that have it.
A 40 GiB volume clones in single-digit milliseconds without allocating new
blocks because both files share extents until written. Without reflinks (for
example, on ext4), sprout reports that it used a slower hole-skipping copy.

Snapshots live inside the instance directory, so `sprout delete` takes them with the
instance; its prompt counts them first. A fork is a *new instance*, identified
by the repository and branch `sprout fork` ran under (or the name it was
given), carrying only the source's `/var` and its
recorded build; credentials, shared and project caches, and ssh material are
re-projected on its first boot like any other instance's. Instance-scoped
caches are part of `/var`, so a fork or a snapshot carries them along with
the rest of the volume.

The sizes `list`, `inspect`, and `snapshot list` report are allocated blocks per
file, which shared extents make look like duplication that isn't there: right
after a fork both images report their full size while together occupying the
space of one. They diverge, and start costing real disk, only as each side is
written.

Because `/var` carries the guest's ssh host keys (`/var/lib/ssh`), a fork
answers with the same host key as its source. That is why host-key trust is
per-instance (`known_hosts` above) rather than shared.
