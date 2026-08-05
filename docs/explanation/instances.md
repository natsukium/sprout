# Instance identity

An instance is identified by the pair **(repository, branch)**, not by its
boot directory. For `list` columns and the on-disk layout, see [instance
state](../reference/instance-state.md).

## Why not the directory, and why not the branch name alone

virtiofs mounts the git toplevel at `/workspace`, but the directory cannot
define identity. Two worktrees of one clone on different branches need
separate instances, while a single worktree switched between branches must
not share one database across those branches.

The branch name alone would collide across clones. Two clones on `main` would
share an instance directory and `/var` without any repository check.

Identity therefore hashes the repository's shared `.git` directory (`git
rev-parse --git-common-dir`, identical from every worktree of one clone)
together with the branch. Two clones on `main` get different hashes; two
worktrees of one clone on different branches get different hashes; the same
branch from any worktree of one clone gets the same hash. The branch name
never enters a filesystem path, so a slash or unicode in it needs no
escaping. Outside a git repository, or on a detached HEAD where there is no
branch to bind an environment to, identity falls back to the worktree path
itself.

## Switching branches in place

Because identity binds to the branch, switching branches in a single
worktree without `git worktree add` does not stop or reattach anything. The
instance keeps running against its own `/var`. Its `/workspace` mount is
live, so it now shows whatever the worktree currently has checked out: a
`feature-x` instance whose worktree is now on `main` runs `feature-x`'s
database against `main`'s files.

`list` reports this as `stale` rather than letting the mismatch pass for
`running`. The state is computed by comparing the instance's branch against
the worktree's current `HEAD`; when they diverge the state is `stale`, and
switching back clears it. `ssh` into a stale instance still works and prints
a one-line warning, since the `/var` is intact and reaching it is often what
you want.

Reaching it needs `-i`, though. Every worktree-scoped command addresses the
branch checked out now, so after a switch `exec`, `stop`, and `status` report
the new branch's environment as stopped or absent while `list` still shows the
previous branch's instance running. Each of them names that instance, and `-i`
reaches it (`sprout exec -i main -- …`); switching the branch back, or `sprout
stop --all`, reclaims it. `open` is the one command that fails quietly: it
prints the new branch's URL, which the router has no instance to answer with.

Stopping on every branch switch would discard warm services during a quick
visit to another branch. Stale instances therefore remain alive until idle
auto-stop reclaims their memory (see [idle
auto-stop](#idle-auto-stop-not-branch-switching)).

## Why there is no ambient selector

`-i` is the only override; no environment variable stands in for it. An
ambient target lets a stray shell export silently redirect a destructive
command, and exempting only the destructive commands would make targeting a
per-command rule again. A script that drives several instances therefore
passes its own variable at the call site (`sprout delete -i
"$SPROUT_INSTANCE" --force`), where the target is visible in the command that
acts on it.

## Running two branches at once

Two instances can mount the same worktree while keeping separate `/var`
volumes. Boot `main` in a worktree that already has a `feature-x` instance and
`up` starts the second one alongside the first, printing a note that names
`feature-x` and how to address it rather than refusing.
The one caveat is that host-side edits are visible to both, and two guests
writing the same workspace file concurrently have no page-cache coherence
guarantee. The usual flow, edit on the host and run in the guest, never
hits this.

## Idle auto-stop, not branch switching

Because instances accumulate, `idle.action` defaults to `stop` after
`idle.after` (two hours by default) without activity. Stop returns the guest's
RAM to the host, and the next
`up` is a fresh boot with only `/var` preserved. Activity means an open SSH
session or a connection bridged through the router; each request restarts
the clock, so the workflow of editing on the host and reloading through the
router does not idle-stop mid-use. A plain `sprout forward` left running against
an otherwise-idle instance does not keep it alive, so a forwarded port cannot
silently pin a VM in memory for hours. Neither does an open browser tab as
such: the router sees connections, not tabs, so once the page's connections
close the clock runs, and the next request wakes the instance.

## Leftovers that outlive their branch

An instance whose worktree or branch is deleted is an orphan. `list` marks it,
and `sprout prune` removes stopped orphans. A branch rename also creates a new
(repository, branch) pair because Git provides no rename history; `prune`
removes the orphaned instance.
