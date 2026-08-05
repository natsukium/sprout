# sprout 🌱

Run one Linux development environment per Git branch.

`sprout` boots NixOS microVMs for local development on Apple Silicon Macs. Each
branch gets an independent instance with its own persistent `/var`, while its
worktree is mounted live at `/workspace`. Open several Git worktrees and run
databases, container stacks, or Kubernetes clusters side by side without
sharing their runtime state or changing the ports they use inside the guest.
It needs Nix with flakes enabled and an `aarch64-linux` builder; `sprout init`
writes the flake, so a project starts without Nix code of its own.

## Example: a feature branch running next to `main`

Suppose the project's dev command starts a web app on port `5173` and
PostgreSQL on port `5432`, and `feature/teams` changes the account schema and
backfills every existing row. You want to click through the branch with
today's version still open beside it. Starting the stack twice on one Mac does
not get you there: the second copy has to move off both ports, and it still
talks to the one PostgreSQL, so the branch's backfill also rewrites what
`main` shows.

Instead, give the branch its own worktree and boot the same stack from each:

```console
# Terminal 1 stays in the main worktree
$ sprout up
building and booting "main" in the background (log: …/up.log) …
VM ready. Enter it with: sprout shell
$ sprout exec -- npm run dev
```

```console
# Terminal 2, starting from the main worktree
$ git worktree add ../project-teams -b feature/teams
$ cd ../project-teams
$ sprout up
building and booting "feature/teams" in the background (log: …/up.log) …
VM ready. Enter it with: sprout shell
$ sprout exec -- npm run dev
```

Neither dev server had to be told anything about the other. Both bind guest
`5173` and `5432`, because each VM has its own network. Each writes to its own
persistent `/var`, so the backfill on `feature/teams` cannot reach `main`'s
database. And each mounts the worktree it was booted from at `/workspace`, so
an edit on the host is live in the matching environment with nothing to sync.

`sprout list` shows the pair (IDs, measurements, and paths vary by host):

```console
$ sprout list
ID            NAME           STATE    UPTIME  CPU  MEM    DISK   WORKSPACE
2493c5c3af70  feature/teams  running  <1m     -    12MiB  67MiB  …/project-teams
887757c37a76  main           running  <1m     -    12MiB  67MiB  …/project
```

To see both in the browser at once, run one router in a third terminal. It
serves every instance on a single host port, addressed by branch name:

```console
$ sprout route serve --port 8080
routing http://<name>.sprout.localhost:8080/ → instances on localhost:8080 (Ctrl-C to stop)
```

```text
http://5173.main.sprout.localhost:8080/           → main :5173
http://5173.feature-teams.sprout.localhost:8080/  → feature/teams :5173
```

Reading a URL left to right: the leading `5173` is the guest port to forward
to, the label next to `sprout.localhost` picks the instance, and `:8080` is
the host port the router bound.

From here on, switching between the old page and the new one is switching
browser tabs. When the comparison is over, `sprout stop` gives an instance's
memory back and leaves its database on `/var` for the next session, and
`sprout delete` discards that branch's runtime state and nothing else. An instance
left alone stops itself after two hours without SSH or router traffic
([`idle.after`](docs/reference/configuration.md)), and a running router boots
it back on the next request.

## Why branch-scoped environments

A branch often needs more than another copy of the source tree. Its database,
containers, cluster state, and background services must also be independent if
two versions are to run at the same time. That independence is what the
following cases have in common:

- **Keep a reference running.** Boot `main` next to the branch you are
  changing and compare the two in a browser, [one URL per
  branch](docs/how-to/route.md), instead of restarting a stack whenever you
  need to see the old behavior again.
- **Try several implementations at once.** Give each candidate its own branch,
  whether they differ in schema design, page layout, or library choice, and
  leave them all up while you decide: [compare branches side by
  side](docs/how-to/compare-branches.md).
- **Run a migration you can undo.** A migration reaches only that branch's own
  `/var` volume, and a [snapshot](docs/how-to/snapshots.md) taken before it
  restores the volume afterwards.
- **Take a review without putting your own work away.** Check the pull request
  out in another worktree and boot it; your instance keeps running, its
  services and database untouched. Reproducing a bug against an older commit
  works the same way.
- **Reuse an environment that was expensive to set up.** When the cost sits in
  a seeded database or a converged cluster rather than the build, [`sprout
  fork`](docs/how-to/snapshots.md#hand-an-environment-to-another-branch) clones
  that volume into a new branch instead of repeating the setup.

Container tooling on macOS draws this boundary elsewhere. Docker Desktop,
OrbStack, and colima run every container inside one shared Linux VM: a
compose project can keep a branch's containers apart, but all branches still
compete for the host ports they publish, share that VM's kernel and container
runtime, and none has a volume to snapshot or fork as one unit; a workload
that assumes a machine of its own (systemd as PID 1, kernel modules, a k3s
node) needs per-tool workarounds. Apple's `container` boots a VM per
container, but the unit is still one container, not a branch's stack with its
services and state. sprout makes the branch the unit: each gets a whole VM,
so it owns everything its stack can touch.

`sprout` treats the repository and branch as the instance identity, so commands
can derive their target without manually assigned VM names.

The guest is a full NixOS system declared in the project flake. It can run
systemd, Docker or Podman, k3s, and kernel modules. The flake lock pins the
guest definition, while host credentials and build caches are available only
when the project declares them.

## Quick start

You need Apple Silicon macOS, Nix with flakes enabled, and an
[`aarch64-linux` builder](examples/README.md#building-on-apple-silicon).
Check the host before booting a VM:

```console
$ nix shell github:natsukium/sprout
$ sprout doctor
```

In a repository without a `flake.nix`, generate a minimal VM definition and
boot it:

```console
$ sprout init
$ git add flake.nix     # make the new file visible to Nix
$ sprout up
$ sprout exec -- uname -a
$ sprout shell
```

`sprout init` does not overwrite an existing flake. The [getting-started
tutorial](docs/tutorials/getting-started.md) shows how to add the same
configuration to an existing flake or one that does not use flake-parts.

Once the configuration is committed, run `sprout up` from another worktree to
boot an independent instance as in the example above. The router reaches
running HTTP services by instance name; start it with
`sprout route serve --port 8080`. Use `sprout forward` for raw TCP ports.

## Capabilities

- **Independent branch state:** one persistent `/var` volume per instance,
  with copy-on-write snapshots and forks.
- **Declarative guests:** project flake options define resources, credentials,
  caches, and readiness; ordinary NixOS modules define packages and services.
- **Worktree-aware commands:** `up`, `shell`, `exec`, `stop`, and `delete`
  target the current repository and branch by default.
- **Full Linux stacks:** run systemd services, nested container runtimes, or a
  local k3s cluster inside the VM.
- **Named HTTP routing:** reach several branches through one host port without
  editing DNS or `/etc/hosts`.
- **Scoped host integration:** the guest sees the worktree, the read-only
  host Nix store, and beyond those only the credentials and caches the
  project declares.

## Documentation

- [Getting started](docs/tutorials/getting-started.md)
- [How-to guides](docs/README.md#how-to)
- [CLI reference](docs/reference/cli.md)
- [Configuration reference](docs/reference/configuration.md)
- [Instance identity](docs/explanation/instances.md)
- [Architecture and security boundary](docs/explanation/architecture.md)
- [Runnable examples](examples/)
- [Operating sprout from a coding agent](skills/sprout/SKILL.md) — the
  agent skill: non-interactive rules, first-time setup, troubleshooting

## Scope and status

`sprout` supports Apple Silicon macOS and `aarch64-linux` guests. It is in the
0.1.0 pre-release line, so the CLI and configuration may change between
releases. See the [compatibility and release policy](docs/reference/compatibility.md).

`sprout` is a development environment, not a sandbox for untrusted code. With
`workspace = true`, root in the guest can modify the checkout and its Git
metadata. Enable only the shares and credentials the guest needs; the
[architecture documentation](docs/explanation/architecture.md#what-the-guest-can-reach)
lists every path across the VM boundary.

## Development

Run `direnv allow` or `nix develop` to enter the contributor shell. `nix flake
check` runs formatting and evaluates the guest modules. `go test ./...` runs the
Go suite and checks that fenced Nix snippets parse and documented `sprout`
commands still match the CLI. `dev/check-examples.sh` evaluates each runnable
example flake without building it; CI runs all three checks.

## License

`sprout` is distributed under the [Apache License, Version 2.0](LICENSE).
