# Architecture: Nix defines, Go executes

Each instance uses one static Go binary instead of a gvproxy process, a vfkit
wrapper, and a separate supervisor. Nix owns the VM definition; Go owns the
runtime.

## The seam

Evaluating `sprout.vms.<name>` produces two artifacts, not one:

```
nix build .#sproutConfigurations.<name>
└── result/
    ├── runner          # microvm.nix vfkit runner, placeholders baked in
    └── manifest.json   # host-side actions for the binary to perform
```

(The guest NixOS system behind the bundle is also exposed as
`nixosConfigurations.sprout-<name>`, so stock tooling can introspect it:
`nix eval .#nixosConfigurations.sprout-<name>.config.…`.)

The runner is an unmodified microvm.nix artifact with placeholder paths in
its device arguments. The manifest lists which placeholder maps to which
runtime value. At `up` time the binary substitutes the placeholders into a
per-instance `run.sh`, so one build serves any number of instances and the
runner stays upstream-compatible with no forked VM logic.

The manifest is schema version 1. It is an explicit contract between the Nix
module and the Go client: an unsupported version is rejected before any runner
is started, rather than silently ignoring fields added by a newer flake.

The manifest carries only three strategy primitives (`mount`,
`materialize`, `socket`) with concrete parameters. The binary implements
those primitives and knows nothing about `gh` or `sccache` specifically.
A custom credential or cache requires no Go change, and `flake.lock` pins
built-in behavior. A manifest-declared host command has the same trust boundary
as a `nix develop` shell hook: both execute code from the current flake.

## The daemon

```
sprout up                                          ← returns once the VM is ready
 └─ sprout up --foreground (detached child)         = the daemon, one per instance
     ├─ embeds gvisor-tap-vsock as a library   ← networking, port forwards
     ├─ supervises the microvm.nix vfkit runner ← Nix owns the definition
     │   └─ PTY handling for the serial console (macOS 26 workaround)
     └─ control socket                          ← ssh/forward/stop talk here
```

`up` re-execs itself with `--foreground` and waits only for readiness, so the
command you typed returns to the prompt while the daemon it left behind owns
the VM's lifetime. A supervisor that must own that process (launchd) runs
`--foreground` itself and skips the fork; see [run as a
daemon](../how-to/run-as-daemon.md).

Embedding gvisor-tap-vsock avoids the socket-startup races, orphan cleanup,
and `EADDRINUSE` retries of a separate gvproxy process. The daemon and network
stack therefore share one lifetime and expose an in-process port-forward API.

SSH uses no host port at all. `sprout shell` reaches the guest through the
in-process network stack with `ProxyCommand sprout dial-stdio`, the same
pattern as `docker system dial-stdio`. Nothing is allocated on the host, so
nothing collides and nothing has to persist across boots. The graceful-stop
sequence (vfkit REST stop, then SIGTERM, then SIGKILL, with waits for the
Virtualization.framework XPC teardown) lives in one tested place, because
killing too early orphans the XPC helper.

## What the guest can reach

The guest crosses the VM boundary through these paths:

- **Outbound internet**, NATed through the in-process network stack, like any
  VM with a user-mode network.
- **DNS**, answered by a resolver the same stack runs at the gateway. The
  guest names that address in `/etc/resolv.conf` and runs no resolver of its
  own, because the file does not stay in the guest: kubelet and docker copy it
  into the network namespaces they create, where a loopback nameserver —
  systemd-resolved's `127.0.0.53`, or a local dnsmasq — addresses the copier
  rather than the resolver. Naming an address that means the same thing from
  every namespace is what makes DNS work in a pod without the workload
  knowing anything about sprout. `dns.wildcardDomains` extends that resolver
  with domains whose subdomains answer `127.0.0.1`, so the router's
  per-branch names resolve inside the guest as well as outside it.
- **The host's loopback**, only if the definition sets `hostLoopback = true`:
  the guest then reaches every `127.0.0.1` listener on the host via
  `192.168.127.254`. This is off by default because "every listener" means
  dev servers, debug ports, and other instances' forwards alike; a guest
  running semi-trusted workloads (an agent, a dependency's test suite)
  should not see them.
- **The host's Nix store**, read-only, in every guest regardless of what the
  project declares: the guest's own system closure lives in the host store
  (`storeOnDisk = false`), so mounting it is what lets one build boot without
  packing a disk image per guest change. `writableStore` overlays guest
  writes onto the instance's own `/var` volume; nothing written through it
  reaches the host store. The remount caveat below applies here with one
  sharpening: a multi-user Nix store is root-owned, so a guest that remounts
  the share writable still cannot alter it, but a single-user install's
  store belongs to the same user the VM process runs as, and a compromised
  guest could then tamper with store paths the host user later executes.
- **The per-instance data directory**, mounted at `/run/sprout`: the SSH
  `authorized_keys`, `instance.env`, and any materialized credentials the
  definition declares. The host writes it at boot; it exists so per-instance
  values reach the guest without being baked into the shared build.
- **The declared shares**, with a caveat: virtiofs carries no read-only flag
  on macOS, so `readOnly` is enforced by the guest's own mount options. Root
  inside the guest can remount and write, so treat `readOnly` as protection
  against accidents, not against a compromised guest, and prefer
  `materialize` or `socket` for credentials the guest must never alter.
  `workspace = true` mounts two of them: the worktree at `/workspace`, and
  the clone's git-common-dir, which a linked worktree's `.git` file points
  at. Guest git works because of the second share, and it is also why guest
  root can rewrite the clone's shared git metadata, not just the mounted
  worktree.
- **Shared caches**, which are declared shares with one extra property: the
  same writable host tree is mounted by every instance — across projects —
  that declares the same cache name. A semi-trusted workload in one guest
  can therefore poison artifacts (compiled objects, packages) a sibling
  project's guest later builds against. This is why `shared` is not the
  default scope: `project` keys the tree by the clone's git-common-dir, so
  the blast radius stops at branches of one repository, and only entries
  whose contents are addressed by their own inputs — sccache keys on
  compiler inputs, the pnpm store on package content — earn the wider
  reach. `scope = "instance"` leaves the host filesystem out of it: the cache
  becomes a directory on the guest's own volume, so nothing a guest writes
  there is reachable from another instance or from the host.
- **Forwarded credential sockets** (`ssh-agent`): the listener sits on the
  gateway inside this instance's network stack, so no host port opens and no
  other instance can reach it. But *anything* with network access inside
  the guest can, including containers and pods the guest runs, since the
  in-guest firewall is off by default. This is the same reach `ssh -A`
  grants a remote host; withhold the credential from definitions that run
  workloads you would not agent-forward to.

In the other direction, the host reaches the guest only through the daemon's
control socket (0700, per-instance). Materialized credentials exist on the
host disk only while the daemon runs: the daemon deletes them on every exit
it can see, and for the exits it cannot (SIGKILL, a host crash) the next
`sprout stop` or boot sweeps the leftovers before anything else happens.
