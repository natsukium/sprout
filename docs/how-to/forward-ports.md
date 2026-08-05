# Forward ports to a guest

Reach a service running inside a guest from the host. `sprout forward` binds
host ports and pipes each connection into the VM for as long as it runs;
Ctrl-C stops it. Listeners bind loopback only by default, so a forwarded guest
port never becomes reachable from the rest of your network unless you ask for
it with `--bind`.

## Forward while you work

```console
$ sprout forward 8080 4566
forwarding localhost:8080 → vm:8080, localhost:4566 → vm:4566 to this worktree's current branch (Ctrl-C to stop)
```

Use `HOSTPORT:GUESTPORT` when the host port is taken. The guest can keep its
default port while the host uses another:

```console
$ sprout forward 8080:80
```

## Follow the branch you are on

With no `-i`, a forward re-resolves its target on every new connection
to the instance for the current branch of the worktree it was started in.
Leave `sprout forward 8080` running, `git switch main`, `sprout up` the new branch,
and the next request reaches `main`'s VM. An already-open keep-alive
connection stays with the instance it first dialed, so reload the page to
move to the new one.

## Pin to one instance

Pass `-i` (a branch, custom name, or instance ID) to pin the target. Run two
for side-by-side comparison:

```console
$ sprout forward 8080                    # follows the current branch
$ sprout forward -i feature-x 8081:8080  # pinned to feature-x
```

## When the instance stops under it

A forward outlives its target. It keeps the host port bound when the instance
idle-stops, so the port goes on accepting connections and each one fails with
a reset, naming the instance that went away. That is what lets `sprout up`
bring the forward back into service without restarting it, and it is also why
a forward with nothing behind it looks alive from the port alone.

Connections through a forward do not count as activity for [idle
auto-stop](../reference/configuration.md#options), even while open and
carrying traffic: only SSH sessions and router connections reset the idle
clock. A long-lived session held through a forward alone (a `psql` shell, a
debugger) therefore does not keep its instance running past `idle.after`;
keep a `sprout shell` open, route through `sprout route serve` instead, or
set `idle.action = "none"` for that definition.

## Reach a forward from another machine

The default binds `localhost`, which is both loopback families (`127.0.0.1` and
`::1`) on the one port, so a browser reaches the forward whichever address it
resolves `localhost` to. `--bind` changes that address. `--bind
0.0.0.0` binds every interface, so a colleague on your network can reach
the forward at `http://<your-ip>:8080/`, with no name resolution needed on
their side. A specific address (`--bind 192.168.1.10`, or a Tailscale
address) narrows it to that one interface.

```console
$ sprout forward --bind 0.0.0.0 8080:80
```

Only the ports you name are exposed. On macOS, `--bind 0.0.0.0` is also
what lets a non-root process bind a privileged port (<1024): macOS refuses
that against a specific address like `127.0.0.1` but allows it against
`0.0.0.0`. A stack that deliberately exposes a conventional HTTP ingress on
port 80 can therefore use:

```console
$ sprout forward --bind 0.0.0.0 80
```

This is reachable from the network, not just the host. Prefer an unprivileged
loopback port for an ordinary development server.
