# Troubleshooting and cleanup

Read this when a command fails, blocks, or an instance is in a state
the normal loop does not cover.

## Commands that prompt

`delete`, `prune`, and `snapshot restore` confirm on stdin. In a
non-interactive session they block forever — pass `--force`, which
suppresses only the prompt and never widens the target set.

`delete` permanently destroys the instance's persistent `/var` and all
its snapshots. Use it only when the user asked for the deletion; when
the goal is just to free memory, `stop` keeps `/var` and never prompts.

## A failed boot

`sprout up` prints the `up.log` path it writes build output to, and
exits non-zero on failure. A readiness timeout is also non-zero, but
the detached daemon keeps booting: check `sprout status --json` first;
a `booting` instance may yet come up, and `sprout stop` it if it
should not stay running. For a real failure, in order:

```sh
sprout logs -n 200      # runner + console logs (a build failure is only in up.log, above)
sprout doctor --build   # host prerequisites, incl. proving the aarch64-linux builder
```

A broken flake surfaces as Nix's own error in `up`'s output — `status`
deliberately never evaluates the flake, so it stays instant and cannot
show the error.

## States outside the normal loop

| State | Meaning | Move |
| --- | --- | --- |
| `stale` | running, but the worktree checked out a different branch | you are probably targeting the wrong instance — pass `-i`, or `stop` it if the old branch's VM is no longer wanted |
| `orphan` | stopped, and its worktree or branch is gone | `sprout prune --force` deletes every orphan; a stopped instance you could return to is left alone |

Renaming a branch mints a new instance identity and orphans the old
one — the old instance's `/var` is not carried over. `sprout fork` from
the orphan onto the new branch recovers the state, then `prune` clears
the orphan.

## `exec` refuses because the instance is stopped

`exec` and `shell` never auto-start. The error offers the right verb:
`sprout up` (rebuilds if the definition changed), or `sprout start`
when the recorded build is still in the Nix store (no rebuild, no
flake, works from any directory).

## A routed URL answers 502

`"<name>" is running but nothing answered on guest port N` means the
instance is up, is past the first two minutes after its boot, and that
port refused the connection — never that the instance is down (a stop
landing mid-request, or a port still opening right after a boot, gets a
reloading 503 instead). Either the port is wrong (the bare name targets
guest :80; put the port in front, `http://5173.<label>.<domain>/`) or
the server is bound to the guest's `127.0.0.1`, which the router cannot
reach because it arrives over the guest's network interface. Confirm
before guessing, then bind `0.0.0.0`:

```sh
sprout exec -- ss -ltnp
```

## Guest looks low on resources

`sprout list` CPU/MEM are host-side occupancy (hypervisor footprint,
high-water mark), not guest-internal usage. Look inside instead:

```sh
sprout exec -- free -m
sprout exec -- df -h /var
```

## Cleanup

```sh
sprout prune --force            # delete every orphan, nothing else
sprout delete --force           # this instance: /var + snapshots, gone
sprout delete --project --force # every instance of this repository
sprout delete --all --force     # every instance on the host — only on explicit user request
sprout cache list --json        # shared build caches
sprout cache delete NAME        # drop one cache across every arch tree
```
