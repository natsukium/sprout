# Architecture: Nix defines, Go executes

Each instance uses one static Go binary instead of a gvproxy process, a
hypervisor wrapper, and a separate supervisor. Nix owns the VM definition; Go owns the
runtime.

## The seam

Evaluating `sprout.vms.<name>` produces two artifacts, not one:

```
nix build .#sproutConfigurations.<system>.<name>
└── result/
    ├── runner          # microvm.nix runner for the host's backend, placeholders baked in
    └── manifest.json   # host-side actions for the binary to perform
```

(The guest NixOS system behind the bundle is also exposed as
`nixosConfigurations.sprout-<system>-<name>`, so stock tooling can introspect it:
`nix eval .#nixosConfigurations.sprout-<system>-<name>.config.…`.)

The runner is an unmodified microvm.nix artifact with placeholder paths in
its device arguments. The manifest lists which placeholder maps to which
runtime value. At `up` time the binary substitutes the placeholders into a
per-instance `run.sh`, so one build serves any number of instances and the
runner stays upstream-compatible with no forked VM logic.

The manifest is schema version 2. It is an explicit contract between the Nix
module and the Go client: an unsupported version is rejected before any runner
is started, rather than silently ignoring fields added by a newer flake.

The manifest names the host and guest systems and the VM backend that boots
the runner. A backend is a `kind` (`vfkit`, `qemu`) plus values from a few
shared vocabularies, each of which the binary implements once:

| Field | Values |
| --- | --- |
| `network.transport` | `vfkit-unixgram`, `qemu-stream` |
| `control.protocol` | `vfkit-rest`, `qmp` |
| `console.mode` | `pty-announce` (vfkit announces a PTY), `stdio` (runner output is the console) |
| `sidecars` | host processes started before the runner, each ready once its socket appears |

The kind is identity — which hosts can run it, how its VM process is
recognised, which diagnostics apply, and whether existing state may be reused
— while behaviour comes from the vocabulary values. Every socket a backend
uses is a bare name (`net.sock`) that the binary resolves inside the short
socket directory and substitutes as an absolute path for the shared
placeholder `/sprout/placeholder/sock/<name>`; names must be a single path
component and distinct from each other and from `control.sock`.

Under QEMU every virtiofs share is served by its own `virtiofsd` sidecar,
which the bundle declares with the same placeholders as the runner: a share's
source appears only in its sidecar's arguments, so substitution covers both.
The daemon starts the sidecars before the runner and holds the boot until
each has created its socket; a sidecar that exits first fails the boot with
its log tail. Each runs unprivileged in its own user namespace
(`--sandbox=namespace`), with every guest uid and gid mapped to the host
user, so files the guest creates or chowns stay the user's own. The sidecars
die with the daemon however it exits, and one that exits while the guest
runs stops the VM.

Validation and boot are separate steps. A manifest whose kind this host
cannot run is refused by name ("this bundle targets qemu on x86_64-linux;
this sprout on aarch64-darwin supports vfkit"). A manifest that is valid for a
backend whose operations this binary does not implement is refused before
any instance state is touched. A version-1 manifest, which
predates the backend fields, is read as the vfkit backend on
`aarch64-darwin` and refused on Linux.

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
     ├─ supervises the microvm.nix runner      ← Nix owns the definition
     │   ├─ vfkit: PTY handling for the serial console (macOS 26 workaround)
     │   └─ QEMU: one virtiofsd sidecar per share, started first
     └─ control socket                          ← ssh/forward/stop talk here
```

The runner is vfkit on macOS and QEMU on Linux; the rest of the tree is the
same on both.

`up` re-execs itself with `--foreground` and waits only for readiness, so the
command you typed returns to the prompt while the daemon it left behind owns
the VM's lifetime. A supervisor that must own that process (launchd or systemd) runs
`--foreground` itself and skips the fork; see [run as a
daemon](../how-to/run-as-daemon.md).

Embedding gvisor-tap-vsock avoids the socket-startup races, orphan cleanup,
and `EADDRINUSE` retries of a separate gvproxy process. The daemon and network
stack therefore share one lifetime and expose an in-process port-forward API.

The runner's NIC reaches that stack through the backend's network socket,
never a TAP device or host bridge: vfkit sends one Ethernet frame per Unix
datagram, while QEMU's `-netdev stream` connects as a Unix stream client with
length-prefixed frames. Both feed the same virtual switch, so the DHCP lease,
forwards, wildcard DNS and the host-loopback alias behave the same on either.

SSH uses no host port at all. `sprout shell` reaches the guest through the
in-process network stack with `ProxyCommand sprout dial-stdio`, the same
pattern as `docker system dial-stdio`. Nothing is allocated on the host, so
nothing collides and nothing has to persist across boots. The graceful-stop
sequence (a stop request over the backend's control socket, vfkit's REST API
or QEMU's QMP `system_powerdown`, then SIGTERM, then SIGKILL, with waits for
the Virtualization.framework XPC teardown) lives in one tested place, because
killing vfkit too early orphans the XPC helper. On Linux the runner also gets
`PDEATHSIG`, so QEMU exits with its daemon however the daemon dies.

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
  reaches the host store. Under QEMU the store's `virtiofsd` refuses writes
  itself. Under vfkit the remount caveat below applies, with one sharpening:
  a multi-user Nix store is root-owned, so a guest that remounts the share
  writable still cannot alter it, but a single-user install's store belongs
  to the same user the VM process runs as, and a compromised guest could
  then tamper with store paths the host user later executes.
- **The per-instance data directory**, mounted at `/run/sprout`: the SSH
  `authorized_keys`, `instance.env`, and any materialized credentials the
  definition declares. The host writes it at boot; it exists so per-instance
  values reach the guest without being baked into the shared build.
- **The declared shares**, where `readOnly` is enforced differently per
  backend. Under QEMU (Linux) the share's `virtiofsd` runs with
  `--readonly` and refuses writes on the host side, which a remount inside
  the guest cannot lift. Under vfkit (macOS) the virtio-fs device carries no
  read-only flag, so `readOnly` is only the guest's own mount option: root
  inside the guest can remount and write. On macOS, treat `readOnly` as
  protection against accidents, not against a compromised guest, and prefer
  `materialize` or `socket` for credentials the guest must never alter.
  Writable shares are written as the host user on both: vfkit runs as that
  user, and each `virtiofsd` maps every guest uid onto it.
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
