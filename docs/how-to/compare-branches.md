# Compare branches side by side

Two candidate implementations are easier to judge next to each other than one
after the other. Give each candidate a branch and a worktree, and each worktree
boots its own instance with its own database and services; comparing them is
then a matter of addressing them by name.

## Boot one instance per candidate

```bash
git worktree add .worktree/schema-a -b schema-a
git worktree add .worktree/schema-b -b schema-b
```

Start the stack from each worktree, in its own terminal:

```bash
cd .worktree/schema-a
sprout up
sprout exec -- npm run dev
```

Both stacks can bind guest port `5173` and `5432`, because each VM has its own
network. `sprout list` shows one instance per candidate, each with its own
workspace.

## See both in the browser

One router serves every instance, so this is one command for the whole
comparison, not one per branch:

```bash
sprout route serve --port 8080
```

```text
http://5173.schema-a.sprout.localhost:8080/
http://5173.schema-b.sprout.localhost:8080/
```

Put the two URLs in two windows and switch between them. `sprout open --port
8080 -i schema-a 5173` assembles a URL for you instead of recalling the label
rules. [Reach instances by name](route.md) covers those rules and the macOS
restriction behind the `:8080` above.

## Compare what a browser does not show

`sprout exec` runs a command in a chosen instance without allocating a TTY, so
its output pipes and diffs cleanly:

```bash
sprout exec -i schema-a -- pg_dump --schema-only app > /tmp/a.sql
sprout exec -i schema-b -- pg_dump --schema-only app > /tmp/b.sql
diff -u /tmp/a.sql /tmp/b.sql
```

A test suite or a benchmark works the same way: run it in each instance and
compare the two outputs, rather than the current output against your memory of
the last run.

## Start both candidates from the same data

A comparison says something about the change only if both sides start from the
same state. Seed one instance, stop it, then fork its volume into each
candidate instead of booting them fresh:

```bash
sprout stop -i seed
cd .worktree/schema-a
sprout fork -i seed
sprout start
cd ../schema-b
sprout fork -i seed
sprout start
```

`fork` never replaces an existing environment, so fork into a worktree before
its first `sprout up` there, or delete the instance it already has. On a
filesystem with copy-on-write clones the copy costs no disk until the two
volumes diverge ([when it is not
instant](snapshots.md#when-it-is-not-instant)).

A fork copies `/var` verbatim, including any identity the guest's services
wrote into it when they were first set up. [What a fork does not
change](snapshots.md#what-a-fork-does-not-change) says when that rules the
approach out.

## Keep one, delete the rest

```bash
sprout delete -i schema-b
git worktree remove .worktree/schema-b
```

`delete` takes that instance's `/var` volume and its snapshots with it and
leaves Git alone: the branch and its commits survive. Removing the worktree
first also works, and leaves the instance stopped with no worktree to return
to; `sprout list` reports that as `orphan`, and `sprout prune` clears it.

## What several VMs cost

Each running instance holds its own `mem` allocation, 8 GiB by default, so five
candidates at once is a real share of the host. Lower `mem` and `vcpu` in the VM
definition when a comparison needs many instances, or stop the ones you are not
looking at: a stopped instance keeps its `/var` and comes back with `sprout
start`. Instances also auto-stop after `idle.after` (2h by default) without SSH
or routed HTTP activity, so a forgotten candidate releases its memory on its
own. See the [configuration reference](../reference/configuration.md).
