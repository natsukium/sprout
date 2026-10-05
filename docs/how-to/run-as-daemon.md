# Run a supervised instance under launchd or systemd

For a long-lived VM, a self-hosted CI runner rather than a disposable dev
environment, the host modules run the same engine under the host's service
manager instead of a hand-run `sprout up`: the nix-darwin module under launchd,
the NixOS module under systemd. Both take the same options.

## Declare a supervised instance

Import `sprout.darwinModules.default` into your nix-darwin configuration, or
`sprout.nixosModules.default` into your NixOS configuration, then:

```nix
services.sprout = {
  enable = true;
  user = "you";                 # the jobs run as this user (state, Keychain, SSH agent)
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
above a quiet runner stops itself after `idle.after` and the service manager
boots it again. An always-on instance should not race its own idle timer.
A rebuild builds the same bundle as `nix build
.#sproutConfigurations.<system>.<name>`, then the instance's job boots it with
`sprout up --foreground --bundle`. The job restarts the instance whenever it
exits and allows a clean guest poweroff before the service manager escalates
SIGTERM to SIGKILL.

The service manager must supervise `--foreground` because a plain `sprout up`
returns once the VM is ready, which would read as a service exit.

`instances.<name>.autoStart = false` (or `services.sprout.autoStart = false` for
all of them) defines the job without supervising it: it neither starts at boot
nor restarts.

### Under launchd

Each instance is a `launchd.daemons.sprout-<name>` job. It runs as a
**LaunchDaemon** so it survives logout, then drops to `user` via `UserName`.
Per-user state (`~/.local/state/sprout`), the login Keychain used by gh, and
`$SSH_AUTH_SOCK` therefore resolve to that user rather than root. Logs land in
`~/Library/Logs/sprout/<name>.{out,err}.log`.

### Under systemd

Each instance is a `sprout-<name>.service` system unit with `User=` set to its
owner, so it survives logout and starts at boot without lingering, while
per-user state still resolves to that user's home. Logs go to the journal:
`journalctl -u sprout-<name>`.

- **Stop and start** with `systemctl stop|start|restart sprout-<name>`. A
  `sprout stop` of a supervised instance is a clean exit that systemd answers
  by booting it again after 30 seconds, as launchd does.
- **Stopping** signals only `sprout` (`KillMode=mixed`), which asks the guest
  to power off; the runner and its sidecars are not signalled directly, since
  QEMU would quit on SIGTERM without that poweroff. systemd waits up to 90
  seconds before killing what is left. Host shutdown takes the same path, and
  instances stop before the network and the nix daemon do.
- **A failed boot** exits non-zero and is retried every 30 seconds; a rebuild
  that starts one reports the unit as failed.
- **A rebuild** that changes an instance's bundle restarts its unit, which is
  a graceful stop followed by a boot of the new bundle. An instance whose
  bundle is unchanged keeps running.

The units see the host tools sprout shells out to (`git`, `ssh`, `nix`, `ps`)
on `PATH`. Your Nix configuration must still enable `nix-command` and `flakes`
in `nix.settings.experimental-features`, as `sprout doctor` asks of an
interactive install.

## The router

The same module supervises `sprout route serve`, which is what makes port-80 URLs
work without root (the service manager binds the port and hands the socket over):

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
