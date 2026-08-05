# Reach instances by name

`sprout route serve` lets you open several branches' web UIs at once, each at a
stable name, without assigning a host port per instance or touching your DNS:

```console
$ sprout route serve --port 8080
routing http://<name>.sprout.localhost:8080/ → instances on localhost:8080 (Ctrl-C to stop)
```

Now every instance is reachable in the browser by its branch name:

```
http://main.sprout.localhost:8080/          → the "main" instance's :80
http://feat-login.sprout.localhost:8080/    → "feat-login"'s :80
```

(The example picks 8080 because macOS refuses a non-root loopback bind of
port 80; for port-free URLs see [the port 80 problem](#the-port-80-problem-on-macos)
below.)

`*.localhost` always resolves to your own machine by browser and macOS
convention, so this needs no `/etc/hosts` or resolver change. The router
binds one port and forwards each request to the right instance by its
`Host:` header: the same name mapping `sprout shell` uses, so a branch
`feat/login` is reached as `feat-login`.

Leave it running in a terminal (or supervise it; see below). One router
serves every instance; you do not run one per branch. Starting a second
router on an address one already serves for the same `--domain` reports the
running one and exits 0 rather than failing to bind, so a command that starts
the router is safe to re-run — including against the supervised router below.
A different `--domain` on that port is a genuine conflict and still fails.

## Let sprout build the URL

`sprout open` assembles the selected environment's URL and opens it in your
browser:

```console
$ sprout open --port 8080                 # this branch's :80
$ sprout open --port 8080 3000            # this branch's :3000
$ sprout open --port 8080 -i feat-login   # another environment
$ sprout open --port 8080 --print         # just print the URL, for curl or a script
```

`--host-prefix` adds the labels a guest-side ingress splits its own services
apart by, for a project whose UI is not at the bare instance name:

```console
$ sprout open --port 8080 --host-prefix admin.dev  # http://admin.dev.<name>.sprout.localhost:8080/
```

`--port` has to match the port the router serves on; it defaults to 80, like
the router's. If nothing is listening there, `open` says so and names the
command that fixes it, rather than leaving your browser to show a
connection-refused page that never mentions sprout.

## When two instances share a name

Names are host-wide, because a hostname carries no project scope. Two clones
of one repository with `main` checked out both answer to `main`, and the
router refuses that name rather than picking one:

```console
$ curl http://main.sprout.localhost:8080/
<p>Name &#34;main&#34; is ambiguous</p>
<p class="hint">Reach one by its ID instead:<br>
<code>http://152f2dadd3c6.sprout.localhost:8080/</code> (main)<br>
<code>http://2c02705dd5ba.sprout.localhost:8080/</code> (main)</p>
```

An instance ID is a name the router always accepts, and `sprout list` prints
it. You rarely have to copy one by hand: `sprout open` and the router's index
page both fall back to the ID URL for a name more than one instance answers
to, so the links they hand you keep working. Only a name URL you typed or
bookmarked yourself returns the 409 above.

Two *different* names can collide the same way, because sanitizing is lossy:
`feature/foo` and `feature-foo` both answer to the label `feature-foo`. The
router refuses that label as ambiguous just like a shared name, and every
surface handing out a link — `sprout open`, the index page, the guest's
`SPROUT_INSTANCE_LABEL` — falls back to the ID for it in the same way.

## Pick the guest port

The name alone targets the guest's port 80: the sprout convention that 80 is
always your ingress inside the VM. Put a port label in front to reach a
different guest port, for a dev server that listens elsewhere:

```
http://5173.feat-login.sprout.localhost:8080/   → "feat-login"'s :5173
```

## Reach a guest that routes by hostname itself

Some projects run their own hostname router inside the VM (a k3s/traefik
ingress, an nginx or Caddy vhost) that splits several services apart by
`Host:` (`admin.…`, `login.…`, and so on). Put those labels in front of the
instance name and they route to the same instance, arriving with the `Host`
untouched, so the guest's router can demux them:

```
http://admin.feat-login.sprout.localhost:8080/   → "feat-login"'s :80, Host: admin.feat-login.sprout.localhost:8080
http://login.feat-login.sprout.localhost:8080/   → "feat-login"'s :80, Host: login.feat-login.sprout.localhost:8080
```

The label next to `sprout.localhost` is always the instance (a branch name is a
single label); everything to its left is the guest's business. For this to
work the in-guest router must match the branch-suffixed names, either with
suffix-tolerant rules (traefik `` HostRegexp(`^admin\..+`) ``) or by
templating its base domain per branch, so several branches can be served side
by side without their hostnames colliding. A leading port label still works
and composes: `http://5173.admin.feat-login.sprout.localhost:8080/` reaches
that instance's :5173.

Template that base domain from `SPROUT_INSTANCE_LABEL`, which each boot writes
into the guest ([instance identity in the
guest](../reference/instance-state.md#instance-identity-in-the-guest)):

```console
$ sprout exec -- sh -c '. /run/sprout/instance.env && echo $SPROUT_INSTANCE_LABEL'
feat-login
```

The label, not `SPROUT_INSTANCE_NAME`: the name is the branch name, which may
hold characters a hostname cannot, and it is not what the router resolves when
two instances share it. A guest that re-implements the sanitizing rule agrees
with the router only until one of those cases arrives, and the mismatch is
worst where a hostname is compared rather than followed: an OIDC redirect URI
is matched for exact equality, so it fails at the callback, after a login that
appeared to work.

## Which side answered?

A 404 has two sources that need opposite fixes: the router could not resolve
the name (a sprout problem), or it bridged the request and the guest's own
ingress rejected the `Host` it received (a guest configuration one). The status
code alone does not separate them.

Every response the router writes itself carries a `Server` header, and nothing
it bridges does, since a spliced response is passed through byte for byte:

```console
$ curl -si --resolve admin.feat-login.sprout.localhost:8080:127.0.0.1 \
    http://admin.feat-login.sprout.localhost:8080/ | grep -iE '^(HTTP|server:)'
HTTP/1.1 404 Not Found
Server: sprout-route
```

`Server: sprout-route` on a 404 means the request never left the host, so the
name is what to fix. No such header means the guest answered, and its own
router is what has to be taught the branch-suffixed hostname (the section
above).

`--verbose` reports the same thing for every request, plus the target the
router chose:

```console
$ sprout route serve --port 8080 --verbose
routing http://<name>.sprout.localhost:8080/ → instances on localhost:8080 (Ctrl-C to stop)
route: admin.feat-login.sprout.localhost:8080 "GET / HTTP/1.1" -> "feat/login" (152f2dadd3c6) guest:80 bridged
route: ghost.sprout.localhost:8080 "GET / HTTP/1.1" -> no instance answers to label "ghost" (404)
```

A bridged line is logged before the splice begins, so it names the target
rather than the outcome: once the guest owns the response, its status is what
the router no longer sees.

## Waking a stopped instance

A request boots an idle-stopped instance, but does not wait for it. It returns
503 with a waking page straight away, and the request that forwards is a later
one, sent after the guest answers SSH and reaches `sprout-ready.target`. In a
browser this is invisible: the page reloads itself until the instance answers.
From a script it is not, so [poll for the
response](#reaching-a-route-from-curl-or-a-script). Pass `--no-wake` to return
503 without booting anything.

Route traffic counts as activity, so an instance you keep requesting will
not idle-stop under you; once its routed connections close it stops on its
normal `idle.after` schedule and the next visit wakes it again.

## The port 80 problem on macOS

macOS refuses to let a non-root process bind `127.0.0.1:80` (a specific
address on a privileged port), so the bare `sprout route serve` above needs one of
three choices; the command prints them if the bind fails:

- **An unprivileged port:** `sprout route serve --port 8080`. URLs then include
  it: `http://feat-login.sprout.localhost:8080/`.
- **Bind all interfaces:** `sprout route serve --bind 0.0.0.0`. macOS *does* allow a
  non-root bind of `0.0.0.0:80`, so the port-80 URLs work, but the router
  is then reachable from your LAN, where anyone who sends a matching `Host`
  header can reach every instance's every port. That is a broader exposure
  than `sprout forward --bind 0.0.0.0` (which opens only the one port you
  name), so do it only on a trusted network. To let a colleague reach a
  preview, prefer `sprout forward --bind 0.0.0.0 8080:80`: it needs no name
  resolution on their side (`*.sprout.localhost` would resolve to *their*
  loopback, not yours) and exposes just that port.
- **A launchd daemon** that binds `:80` for you and hands the socket to a
  router running as your user (`services.sprout.route`, below). This is the only
  way to keep loopback-only, root-free, port-80 URLs.

Take the first for a one-off and the third for daily use; a router left on
`0.0.0.0` keeps every instance open to the network long after the reason for it
has passed.

`--bind` also takes any local address, so `--bind 192.168.1.10` (or a Tailscale
address) exposes the router on just that interface rather than all of them.
The port-80 allowance does not come along: macOS permits the non-root
privileged bind only for `0.0.0.0`, so a specific address needs an
unprivileged port or the launchd module.
Without it the router binds `localhost`, which is both `127.0.0.1` and `::1` on
the one port, since a browser may resolve `localhost` to either.

## Always-on port 80, without root

The nix-darwin module has launchd bind the port as root and hand the socket to
`sprout route serve` running as you:

```nix
services.sprout = {
  enable = true;
  user = "you";
  route.enable = true;
};
```

After `darwin-rebuild switch`, launchd holds the port and starts the router on
the first request. The router then wakes instances as requests arrive and logs
to `~/Library/Logs/sprout/route.{out,err}.log`.

`route.port`, `route.bindAddress`, `route.domain`, and `route.wake` mirror the
command-line flags; `bindAddress` defaults to `localhost`, which covers both
loopback families. It needs no `services.sprout.instances`: the router reaches
instances you started by hand just as well as supervised ones.

The `--launchd-socket` flag this relies on takes its address and port from the
socket launchd bound, so it rejects `--port`/`--bind` and does nothing useful
outside launchd. Set it through the module rather than by hand.

## Starting the router from a script

The router runs until interrupted, so a test harness that starts one has to
stop it too. Started in the background through a wrapper (`nix develop -c`,
`direnv exec`), the PID the script recorded is the wrapper's, and killing it
leaves the router holding the port. The next run reports that router and
returns, so the script goes on against the survivor — serving whatever flags
it was started with, not the ones just passed. Either `exec` the router so it
inherits that PID:

```console
$ nix develop -c sh -c 'exec sprout route serve --port 8080' &
```

or signal the whole process group. A supervised router (above) sidesteps the
question: the script starts nothing and stops nothing.

## If your dev server rejects the host

A dev server with host-header allow-listing (Vite, Next) must permit the
`.sprout.localhost` name. Vite allows `.localhost` subdomains by default; if
yours does not, add the name to its allowlist (for Vite,
`server.allowedHosts`). The router does not rewrite `Host`, so the guest sees
`feat-login.sprout.localhost`.

## Reaching a route from curl or a script

`*.localhost` resolving to `127.0.0.1` is a browser and macOS *resolver*
convention, but the system resolver `curl` uses does not always apply it to
multi-label names like `feat-login.sprout.localhost`. If a plain
`curl http://feat-login.sprout.localhost:8080/` fails to resolve, point it at
loopback explicitly; the router still demuxes on the `Host` header curl
sends:

```console
$ curl --resolve feat-login.sprout.localhost:8080:127.0.0.1 http://feat-login.sprout.localhost:8080/
```

A stopped instance answers your script the same way it answers a browser: 503
and the waking page, in milliseconds, because the router never holds a request
through a boot. The response carries `Retry-After: 2`. A bare guest often
boots in twenty to thirty seconds, but a readiness hook keeps the router
answering 503 until it passes (up to the [readiness
budget](../reference/configuration.md#readiness)), so poll with an explicit
bound rather than looping until success:

```console
$ curl -fs --retry 60 --retry-delay 2 --retry-all-errors \
    --resolve feat-login.sprout.localhost:8080:127.0.0.1 \
    http://feat-login.sprout.localhost:8080/
```

Sixty attempts two seconds apart give the wake two minutes, then fail rather
than hang the script on an instance that never comes up. When the bound
trips, the last status separates two problems waiting treats alike: a 503 is
a boot or readiness hook still in progress (`sprout logs` shows where it
stands), while a persistent 502 means the instance is up but nothing listens
on the guest port, which more waiting does not fix.

`--no-wake` is not a substitute here: it returns 503 forever, until someone
runs `sprout start`.

## What it does not do

Only HTTP can be demultiplexed by name, so `route` is HTTP-only by design.
To reach a raw TCP service (a database, say) from the host, keep using
`sprout forward 15432:5432`, which assigns it an explicit host port.
