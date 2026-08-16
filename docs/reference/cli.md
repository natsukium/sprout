# CLI reference

The CLI uses Docker-family verbs. `up` and `start` return with the VM running;
other commands run in the foreground.

## Selecting an instance

By default, commands select the current worktree's branch within its
repository. Use an explicit selector to override it:

```console
sprout COMMAND [--instance INSTANCE]
```

`-i` is the short form. The value is a name, or an instance ID or unambiguous
ID prefix (as shown by `sprout list`), which resolves from any directory. A
prefix counts as an ID only at four or more hex characters; anything shorter,
or containing a non-hex character, resolves as a name.

Positional arguments are command operands, never instance selectors: ports for
`forward`, a snapshot for `snapshot restore`, or a guest command for `run`.

No environment variable selects an instance implicitly; see [why there is no
ambient selector](../explanation/instances.md#why-there-is-no-ambient-selector).
Scripts pass their own variable visibly:

```console
sprout delete -i "$SPROUT_INSTANCE" --force
```

Creation is repository-scoped, so `sprout up -i main` in two projects boots two
VMs. Commands targeting an existing instance search the host if the current
repository has no match. Ambiguous names return candidate IDs. See [instance
identity](../explanation/instances.md) for default identity derivation.

## Commands

| Command | Description |
| --- | --- |
| `sprout init` | Write a `flake.nix` declaring one VM, so `up` has something to build. Refuses to overwrite an existing flake; `--vm NAME` picks the definition name. In a Git checkout, stage the new file before `up` so Nix can read it. |
| `sprout up` | Build if needed and boot, wait until the VM is ready, and return with it still running. Idempotent, including under concurrency: parallel `up`s booting the same definition converge on a single boot, so no external locking is needed; `up`s carrying different definitions error out rather than reboot each other indefinitely. Rebuilds and reboots in place if the definition changed. "Ready" is SSH up plus the guest's `sprout-ready.target` reached; see the [readiness reference](configuration.md#readiness). |
| `sprout start` | Boot a *stopped* instance from the build a prior `up` recorded: no `nix build`, no flake, no worktree, so it resolves by name or ID from any directory. Always boots the existing build; use `up` to pick up a changed definition. |
| `sprout shell` | Open an interactive shell in the environment. Always allocates a TTY, and starts in `/workspace` when the guest mounts one. Does not start a stopped instance; see below. |
| `sprout exec -- CMD…` | Run a command in the environment. No TTY, so pipes and CI output stay clean; `-t` allocates one for full-screen commands like `htop`. Starts in `/workspace` when the guest mounts one. |
| `sprout ssh config` | Print an `ssh_config` block for the instance, so any ssh_config-driven tool (VS Code Remote-SSH, JetBrains Gateway, `scp`, `rsync`) reaches the guest through the same ProxyCommand. Append it to `~/.ssh/config` and connect to `sprout-<name>`. Note that plain `ssh sprout-<name>` gets the interactive default only. |
| `sprout run -- CMD…` | Boot a throwaway instance, run one command, destroy it. |
| `sprout forward PORT[:GUESTPORT]…` | Forward host ports into the VM while running. Follows the current branch unless `-i` pins it. |
| `sprout open [GUESTPORT]` | Open the selected environment's routed URL in your browser: the router URL assembled for you, instead of recalling the label rules. `GUESTPORT` reaches a port other than the guest's :80; `--host-prefix` puts a guest-side ingress's own virtual host in front of the instance name; `--print` writes the URL instead of opening it. A route label more than one instance answers to yields that instance's ID URL, the only one the router resolves. Needs a running router (`--port` must match its port). |
| `sprout route serve [--port PORT] [--bind ADDR] [--no-wake] [--verbose]` | Reach any instance's HTTP by name at `http://<name>.sprout.localhost/` through one shared host port, with no DNS or `/etc/hosts` edit. A request for a stopped instance wakes it (unless `--no-wake`) and returns 503 with a waking page; a later request forwards, once the guest is ready. Every response the router writes itself carries `Server: sprout-route`; a bridged response is passed through untouched. Starting one where a router already serves the same `--domain` prints that and exits 0, leaving the running router and its flags in place; another `--domain` on that port is an error. HTTP only; raw TCP stays on `forward`. |
| `sprout status` | Describe one environment (its state, definition, and uptime or disk) and name the command most likely wanted next. Also what a bare `sprout` prints, so an environment you have forgotten the state of is one word away. `--json` prints the same facts as an object. |
| `sprout list [-q\|--json] [--project]` | List every instance on this host. `-q` prints only IDs, one per line (`stop` takes one instance per invocation, through `-i` only, so compose with `sprout list -q \| xargs -n1 sprout stop -i`, or use `stop --all`); `--json` prints the listing rows as objects with raw metrics; `--project` narrows to the current repository's instances. The full record is `inspect`'s. |
| `sprout inspect` | Print one instance's full record as JSON: every field of its `instance.json` (bundle, guest IP, PID, …) plus live state, metrics, and `routeLabel`: the hostname label `route` answers to for it, which is the sanitized name, or the ID when more than one instance answers to that label (a shared name, or distinct names sanitizing alike; see [name collisions](../how-to/route.md#when-two-instances-share-a-name)). The single-instance counterpart to `list --json`. |
| `sprout stop [--all\|--project]` | Graceful shutdown; the persistent volume is kept. |
| `sprout delete [--force] [--all\|--project]` | Stop the environment and permanently delete its state, including its persistent `/var` volume and any snapshots. Asks first; `--all` covers every instance on the host, `--project` only the current repository's, and either asks once for the whole set. |
| `sprout prune [--force]` | Delete every orphaned instance: stopped, with its worktree or branch gone (see the states table). A stopped instance you could still return to is left alone. |
| `sprout snapshot create [--live] SNAP` | Save the instance's `/var` volume under `SNAP`. A copy-on-write clone where the filesystem has them, instant and costing no disk until the two images diverge; a full copy otherwise. Requires the instance stopped unless `--live`. |
| `sprout snapshot list [--json]` | List the instance's snapshots, oldest first. `--json` prints them as an array with raw sizes. |
| `sprout snapshot delete SNAP` | Delete one snapshot. The instance and its live `/var` are untouched, so there is no prompt. |
| `sprout snapshot restore [--force] SNAP` | Roll `/var` back to `SNAP`, discarding its current contents. The instance must be stopped; `--force` skips the confirmation. |
| `sprout fork [NEWNAME] [--live]` | Create a new environment seeded with the selected one's `/var` and build. `-i` selects the source; the destination is the positional operand, or the current worktree's branch when omitted: the way to hand an expensive environment to another branch. Requires the source stopped unless `--live`. The destination must not already exist, and there is no `--force`. |
| `sprout logs [-f] [-n LINES]` | Show runner and console logs; `-f` follows. |
| `sprout cache list [--json]` | List host-side build caches, shared and project alike; the PROJECT column is `-` for a shared cache. `--json` prints them as an array with raw sizes, and adds each cache's `path` — the host directory to read, which for a project cache sits under a key derived from the clone. Deleting one is still `cache delete`. |
| `sprout cache delete NAME` | Delete a host-side build cache across every arch tree and every project holding the name. |
| `sprout doctor [--build]` | Check every prerequisite `up` needs (Nix with flakes, an `aarch64-linux` builder, Virtualization.framework, ssh) and print the fix for whatever is missing. `--build` additionally proves the builder chain with a trivial Linux build. |
| `sprout version` | Print the sprout build stamp (`sprout --version` also works). |

## Flags

| Flag | Commands | Meaning |
| --- | --- | --- |
| `--instance NAME`, `-i` | most | Instance name or ID; overrides the branch default. |
| `--tty`, `-t` | `exec` | Allocate a TTY, for full-screen commands like `htop`. `shell` always has one, so it has no such flag. |
| `--foreground` | `up`, `start` | Run the daemon in this process instead of returning once the VM is ready. For a supervisor that must own a process living as long as the VM; see [run as a daemon](../how-to/run-as-daemon.md). |
| `--vm DEF` | `up`, `run`, `init` | The VM definition (`sprout.vms.<DEF>`) to build, or for `init` to declare (default `dev`). Omitted on `up`/`run`: the flake's only definition is used; with several, `dev` if it exists, otherwise an error lists the candidates. |
| `--flake REF` | `up`, `run` | Flake reference to build from; default `.`. |
| `--all` | `stop`, `delete` | Apply to every instance on the host. Mutually exclusive with `-i` and `--project`. |
| `--project` | `stop`, `delete`, `list` | Narrow to the current repository's instances: those whose recorded repository root matches the directory you stand in. On `stop` and `delete`, mutually exclusive with `-i` and `--all`. |
| `--live` | `snapshot create`, `fork` | Copy the volume of a *running* instance. The result is crash-consistent (what a power cut would have left) and is refused on a filesystem without copy-on-write clones, where the copy could not be atomic. |
| `--port PORT` | `route serve`, `open` | For `route serve`, the host port to bind; default 80. URLs then carry it (`http://<name>.sprout.localhost:PORT/`). For `open`, the port the router is already serving on; the two have to agree. |
| `--bind ADDR` | `forward`, `route serve` | Address to bind; default `localhost`, which binds both loopback families (`127.0.0.1` and `::1`) on the one port, since a browser may resolve `localhost` to either. A literal address binds only that address. A non-loopback address (`0.0.0.0`, a LAN/Tailscale IP) makes the listener reachable from that network. `0.0.0.0` is also the one non-root way onto a privileged port (<1024) on macOS; the restriction applies to specific addresses, so a LAN or Tailscale address below 1024 still needs root. For `forward` the exposure is bounded to the ports you name; for `route` it is broad: every instance's every port becomes reachable by Host header. |
| `--no-wake` | `route serve` | Do not auto-start a stopped instance when a request arrives for it. |
| `--verbose` | `route serve` | Log one line per request to stderr: the `Host` received, the request line, and what it resolved to (the instance and guest port it was bridged to, or the status the router answered with itself). |
| `--domain SUFFIX` | `route serve`, `open` | Hostname suffix to route; default `sprout.localhost`. |
| `--host-prefix LABELS` | `open` | Hostname labels to place in front of the instance name, for a guest that routes by `Host` itself (`--host-prefix admin.dev` → `http://admin.dev.<name>.sprout.localhost/`). A `GUESTPORT` operand stays leftmost, the only position the router reads it in. |
| `--launchd-socket NAME` | `route serve` | Serve the socket launchd bound under this `Sockets` key instead of binding one, the only way to loopback `:80` without root. Set by `services.sprout.route` and hidden from `--help`; it takes its address and port from launchd, so it refuses `--port`/`--bind`. |
| `--force` | `delete`, `prune`, `snapshot restore` | Skip the confirmation prompt. It suppresses only the prompt: the set of things acted on is unchanged. |
| `--quiet`, `-q` | `list` | Print only instance IDs, one per line, for scripting. |
| `--print` | `open` | Print the URL instead of opening it, for piping into curl or a script. |
| `--json` | `list`, `status`, `snapshot list`, `cache list` | Print machine-readable output with raw (unformatted) metrics: an object for `status`, an array (`[]` when empty) for the rest. |
| `--follow`, `-f` | `logs` | Follow the console log as new output arrives. |
| `--lines LINES`, `-n` | `logs` | Trailing lines to show per log; default 80. |

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | The operation succeeded; for `up` and `start`, the VM is ready. |
| 1 | The operation failed. |
| 2 | The command line was invalid; the message ends with a `Run '… --help' for usage.` pointer naming the failing command. |

`run` and `exec` are the exception to code 1: they pass the guest command's
own exit status through, so a failing test suite's code survives the VM
boundary. A script distinguishes "I typed it wrong" (2) from "it did not
work" (1) on every other command.

## Bulk operations

`--all` selects every instance on this host; `--project` selects only the
instances belonging to the repository you stand in, which is the scope a
per-project cleanup script wants — the state root is host-global, so `--all`
in one project's justfile would also destroy every other project's
environments. Either flag is rejected alongside `-i` (and alongside the other)
rather than resolved one way or the other: a command line naming two scopes,
or a scope and a target, has no reading that is obviously what was meant.

`stop --all` keeps every persistent volume, so it does not ask. `delete --all`
lists the target set and asks once before destroying it:

```console
$ sprout delete --all
  main (a1b2c3d4e5f6)
  feature-x (f6e5d4c3b2a1, 2 snapshot(s))
delete the 2 instance(s) above, including their persistent /var volumes? [y/N]
```

`delete --all` prompts once for the set. `--force` skips the prompt without
changing the targets.

## `sprout up` and `sprout start`

Both boot in the background and return once the VM is ready, printing where to
go next:

```console
$ sprout up
building and booting "main" in the background (log: …/up.log) …
VM ready. Enter it with: sprout shell
```

A development environment is something you leave running while you work in the
same terminal, so occupying that terminal would mean every user opening a
second one. Build output goes to `up.log`, which the boot message names and a
failure names again with the last log line; `sprout logs` shows the runner and
console logs instead. The command's own exit status reports the
outcome, so `sprout up && sprout exec -- …` is sound in a script: zero means
the VM is ready.

A non-zero exit has three causes, and they differ in what is left behind. A
failed build and a daemon that exits leave nothing running; a readiness
timeout only means the wait budget ran out, while the detached daemon boots
on and the VM may become ready after `up` has returned. After a timeout, read
`sprout status` and `sprout logs` before treating the instance as dead, and
`sprout stop` it if a late boot should not stay running.

`--foreground` runs the daemon in this process instead. That is for a
supervisor that must own a process living as long as the VM (launchd, in the
[daemon module](../how-to/run-as-daemon.md)), not for watching the boot, which
`sprout logs -f` does better.

When another branch's instance is already running for this worktree, `up`
prints a note naming it and boots alongside it; the two keep separate `/var`
volumes (see [instance identity](../explanation/instances.md)).

## `sprout list` columns

| Column | Meaning |
| --- | --- |
| `ID` | The instance ID `-i` accepts, whole or as a prefix. |
| `NAME` | The branch (or the name the instance was created under), unsanitized. |
| `STATE` | See the state table below. |
| `UPTIME` | Time since the daemon started, minute granularity. |
| `CPU` | Host CPU the VM's process tree is using (a decaying average). |
| `MEM` | Host memory the VM occupies (vfkit's footprint). |
| `DISK` | Allocated (sparse) size of the guest's `/var` volume. |
| `WORKSPACE` | The worktree the instance was booted from. |

`CPU` and `MEM` are host-side occupancy, not guest-internal usage: `MEM` is how
much host RAM the hypervisor holds, which rises to a high-water mark and does
not shrink when the guest frees memory. To judge whether a guest could run with
a smaller `mem` allocation, look inside it instead, e.g. `sprout exec -- free -m`.
Both read `-` for a stopped instance, and when the sample failed.

## `sprout list` states

| State | Meaning |
| --- | --- |
| `booting` | Daemon is up; the guest has not answered SSH yet. |
| `running` | Reachable, and the worktree still has this branch checked out. |
| `stale` | Running, but the worktree checked out a different branch. |
| `stopped` | No daemon; state on disk, `/var` preserved. |
| `orphan` | Stopped, and its worktree or branch is gone. Cleared by `prune`. |

## `sprout status`

`status` answers "what am I looking at, and what do I do next?" for one
environment. It is what a bare `sprout` prints, so the CLI stays usable when you
do not remember a command name.

```console
$ sprout
Environment: main
State:       stopped
Definition:  dev
Disk:        2.1GiB

Start it with:
  sprout up
```

The final block is chosen from the state: a running instance says how to enter
it, a booting one where to watch the boot, an orphan that `prune` is what is
left to do. A checkout with no instance yet reports that, and distinguishes the
two reasons: a `flake.nix` that has simply never been booted (`sprout up`), or a
directory with no `flake.nix` at all, which needs a definition written first.
That check is a `stat`, never an evaluation, so `status` stays instant and a
broken flake surfaces from `sprout up` with Nix's own error rather than here.
It also looks only in the current directory: from a subdirectory of a
repository whose `flake.nix` sits at the root, `status` reports no flake and
suggests `sprout init`, which would write a second flake there. Run it from
the directory holding `flake.nix`.

`--json` prints the same facts as an object, with raw units:

```console
$ sprout status --json
{
  "instance": "main",
  "id": "6f21c0a4b3d5",
  "state": "stopped",
  "definition": "dev",
  "uptimeSec": 0,
  "diskBytes": 2254857728,
  "workspace": "/Users/you/src/project"
}
```

## `sprout run`

`run` composes the other verbs: it boots a detached daemon under a random
ephemeral name, waits until the instance is ready (the same
[readiness](configuration.md#readiness) `up` waits on, so a readiness hook
and its budget gate `run` too), runs the command as an SSH child so the
command's exit code becomes `sprout`'s, and tears the instance down on every
exit path. A broken build fails fast instead of waiting out the readiness
timeout.

## `sprout shell` and `sprout exec`

`shell` is interactive; `exec` runs a command:

```console
$ sprout shell                  # interactive shell, always a TTY
$ sprout exec -- npm test       # run a command, no TTY
$ sprout exec -t -- htop        # run a command that needs one
$ sprout exec -- sh -c 'cd examples/podman && podman-compose ps'
```

A guest command always begins after `--`. The boundary is unconditional so it
is not a rule to re-evaluate per invocation; a per-invocation rule would be
got wrong the first time a guest command grows a flag of its own. Argument
boundaries after it are preserved, including the single compound argument to
`sh -c` above. Shell operators are syntax only when passed explicitly through
a shell this way.

Neither starts a stopped instance:

```console
$ sprout shell
sprout: instance "main" is stopped; start it with: sprout up
```

Wake-on-access is a property of the HTTP router (see
[route](../how-to/route.md)), not a general lifecycle rule: an entry point
that silently spent thirty seconds booting would be the surprising one. When
the recorded build is still in the Nix store, the error also offers `sprout
start`, which skips the rebuild. When another branch's instance is still
running for this worktree, it names that instance and the `-i` that reaches
it (see [instance identity](../explanation/instances.md)).

## `sprout ssh config`

`sprout shell` reaches a guest with no host ports, by handing system `ssh` a
`ProxyCommand`. Any other ssh_config-driven tool can use the same route:
`sprout ssh config` prints a `Host` block you drop into `~/.ssh/config`.

```console
$ sprout ssh config >> ~/.ssh/config
$ ssh sprout-main            # or point VS Code Remote-SSH / scp / rsync at it
```

The instance need not be running when you generate the block; the
`ProxyCommand` cannot dial until `sprout up` boots it. Note that renaming a
branch mints a *new* instance identity and orphans the old one (see
[instance identity](../explanation/instances.md)); after a rename, generate
a block for the new instance, since the old block points at the orphan.

## Shell completion

`sprout` generates its own completion script for fish, bash, zsh, and
PowerShell. The script calls back into the binary, so instance IDs and names,
snapshot names, and cache names are always the live ones: no per-shell copy
of any list.

```console
$ sprout completion fish | source                                  # fish, this session
$ sprout completion fish > ~/.config/fish/completions/sprout.fish     # fish, permanently
$ source <(sprout completion bash)                                 # bash
$ sprout completion zsh > ~/.zsh/completions/_sprout                  # zsh (on $fpath)
```

`sprout completion --help` documents the per-shell installation paths.
