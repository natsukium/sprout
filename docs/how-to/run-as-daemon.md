# Run a supervised instance under launchd

For a long-lived VM, a self-hosted CI runner rather than a disposable dev
environment, the nix-darwin module runs the same engine under launchd
instead of a hand-run `sprout up`.

## Declare a supervised instance

Import `sprout.darwinModules.default` into your nix-darwin configuration, then:

```nix
services.sprout = {
  enable = true;
  user = "you";                 # launchd runs as this user (Keychain, state, SSH agent)
  instances.runner-1 = {
    vcpu = 8;
    mem = "16GiB";
    modules = [ ./runner-guest.nix ];
    credentials.gh.enable = true;
    idle.action = "none";       # see below
  };
};
```

Each instance takes the `sprout.vms.<name>` options inline, defaults
included: `idle.action` still defaults to `"stop"`, so without the `"none"`
above a quiet runner stops itself after `idle.after` and launchd boots it
again — an always-on instance should not race its own idle timer. `darwin-rebuild`
builds the same bundle as `nix build .#sproutConfigurations.<name>`, then a
`launchd.daemons.sprout-<name>` job boots it with `sprout up --foreground
--bundle`. The job restarts on failure and allows a clean guest poweroff before
launchd escalates SIGTERM to SIGKILL.

launchd must supervise `--foreground` because a plain `sprout up` returns once
the VM is ready, which launchd would treat as a service exit.

The job runs as a **LaunchDaemon** so it survives logout, then drops to `user`
via `UserName`. Per-user state
(`~/.local/state/sprout`), the login Keychain used by gh, and `$SSH_AUTH_SOCK`
therefore resolve to that user rather than root. Logs land in
`~/Library/Logs/sprout/<name>.{out,err}.log`.

## The router

The same module supervises `sprout route serve`, which is what makes port-80 URLs
work without root (launchd binds the port and hands the socket over):

```nix
services.sprout.route.enable = true;
```

The router needs no `instances` and also reaches instances started by hand.
See [reach instances by name](route.md#always-on-port-80-without-root).

## When to use this instead of `sprout up`

Use `sprout up` for anything you start and stop by hand. Reach for the daemon
module only when an instance must survive logout and reboot and restart on
failure. A dev VM auto-stops when idle (see [instance
identity](../explanation/instances.md#idle-auto-stop-not-branch-switching));
a supervised runner is the opposite case, kept up on purpose.
