---
name: sprout
description: Operate sprout — branch-scoped NixOS microVM development environments — from a coding agent, non-interactively. Boot and stop instances, run build/test commands inside the guest with `sprout exec`, read state with `--json`, and snapshot before risky changes. Use whenever a task must run inside the project's sprout VM (Linux-only services, Docker/Podman, k3s, integration tests), whenever the user mentions sprout, an instance, the guest, or the VM, or before running project services that the flake declares. Never run `sprout shell` — it is interactive-only.
allowed-tools: Bash Read Grep
---

# Operating sprout from an agent

An instance is identified by repository × branch: the current worktree
selects the instance, so `cd` is the selector. `-i NAME|ID` overrides it
(resolves host-wide, from any directory). There is deliberately no
environment variable selector; scripts pass `-i` explicitly.

Command grammar and flags are not restated here: `sprout --help` maps
the verbs, and `sprout COMMAND --help` is authoritative for each one.
Run it whenever a flag or operand is in doubt. This file carries only
what help output cannot tell you: the rules of driving sprout from a
non-interactive session.

This file is the normal flow. Read the neighbors when you leave it:

- [setup.md](setup.md) — first time on a host, or a repository without
  a sprout definition in its `flake.nix`.
- [troubleshooting.md](troubleshooting.md) — a failed or hanging boot,
  a `stale`/`orphan` state, a command blocked on a prompt, cleanup.

## Rules for non-interactive sessions

1. **Never `sprout shell` or `sprout exec -t`.** Both allocate a TTY and
   hang a non-interactive session. Run guest commands with
   `sprout exec -- CMD`; its output is pipe-clean and its exit code is
   the guest command's.
2. **Everything after `--` reaches the guest verbatim.** Shell syntax
   needs an explicit shell: `sprout exec -- sh -c 'cd sub && make'`.
3. **`forward`, `route serve`, and `logs -f` run until killed.** Start
   them in the background, never as a blocking call. Through a wrapper
   (`nix develop -c`, `direnv exec`) the PID you recorded is the wrapper's,
   so kill the process group or `exec` the sprout command inside it;
   otherwise the listener outlives it. A stray `forward` then blocks the
   next run's bind; a stray `route serve` is reported and left alone, so
   the next run exits 0 against a router whose flags you did not choose.
4. **Exit codes are trustworthy, but a timeout is not a death.**
   `sprout up && sprout exec -- …` is sound: zero means the VM is ready.
   Non-zero means a failed build, a daemon that exited, or a readiness
   timeout. After a timeout the detached daemon keeps booting and the VM
   may become ready late; check `sprout status --json`, and `sprout stop`
   the instance if it should not stay running.

## Read state before acting

```sh
sprout status --json     # current worktree's instance: state, definition, disk
sprout list --json       # every instance on the host
sprout inspect           # one instance's full record: guest IP, PID, bundle
```

`running` → go ahead with `exec`. `stopped` → `sprout up` first; `exec`
does not auto-start. `booting` → wait, watching `sprout logs -f` in the
background. Any other state → [troubleshooting.md](troubleshooting.md).

## The normal loop

```sh
sprout up                        # idempotent; rebuilds and reboots if the definition changed
sprout exec -- CMD               # runs in /workspace inside the guest
sprout stop                      # free memory; /var is kept, no prompt
```

For a one-off command that needs no persistent state, skip the loop:

```sh
sprout run -- CMD                # throwaway instance: boot, run once, destroy
```

`sprout delete` is not the counterpart of `up`: it permanently destroys
the instance's `/var` and snapshots, and prompts on stdin. Reach for it
only on explicit user request, with `--force` (see
[troubleshooting.md](troubleshooting.md#commands-that-prompt)).

## Guest state: snapshot, restore, fork

Before a risky change inside the guest (schema migration, destructive
test), save `/var`:

```sh
sprout snapshot create pre-migration --live   # --live: instance may stay running (crash-consistent)
sprout snapshot list --json
sprout snapshot restore pre-migration --force # roll back; instance must be stopped first
```

`sprout fork NEWNAME` seeds a new branch's environment from the selected
one's `/var` and build — use it instead of re-running expensive setup on
a new branch.

## Reaching guest services

```sh
sprout open --print [GUESTPORT]   # routed URL for curl (needs `sprout route serve` running)
sprout open --print --host-prefix admin.dev # same, with a guest ingress's own virtual host
sprout forward 8080:80            # raw TCP; foreground — run in background, kill to stop
sprout ssh config                 # ssh_config block for scp/rsync/Remote-SSH
```

Both bind loopback only (both families) unless `--bind` says otherwise.

Never derive a route hostname by sanitizing the branch name yourself. The
label sprout actually routes on is `routeLabel` in `sprout inspect`, and
`SPROUT_INSTANCE_LABEL` in `/run/sprout/instance.env` inside the guest; it
falls back to the instance ID when more than one instance answers to the
same label.

A 404 from a routed URL has two sources. `Server: sprout-route` in the
response head means the router answered and never reached the guest;
without it the guest's own ingress answered. `sprout route serve --verbose`
logs each request's Host, instance, and guest port.

## Bulk operations

```sh
sprout list -q | xargs -n1 sprout stop -i   # stop takes one instance per invocation, via -i only
sprout stop --project                    # this repository's instances; keeps every /var, no prompt
sprout stop --all                        # every instance on the host
```

`--all`, `--project`, and `-i` are mutually exclusive. Prefer `--project`
for cleanup: the state root is host-global, so `--all` also reaches every
other project's instances. Bulk *deletion* is in
[troubleshooting.md](troubleshooting.md#cleanup).
