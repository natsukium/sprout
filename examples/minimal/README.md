# Minimal

Use this default VM definition to verify your setup before trying the
[podman](../podman/) or [k3s](../k3s/) examples.

```console
$ cd examples/minimal
$ sprout up
building and booting "main" in the background (log: …/up.log) …
VM ready. Enter it with: sprout shell
$ sprout exec -- uname -a
Linux sprout-dev 6.x.x … aarch64 GNU/Linux
$ sprout delete
```

The result is a NixOS system with systemd, sshd, and the repository mounted at
`/workspace`. The instance is named after the current branch; its guest hostname
is `sprout-dev`, after the `sprout.vms.dev` definition. The first `sprout up` uses the
`aarch64-linux` builder described in the [build
notes](../README.md#building-on-apple-silicon); later boots reuse the cached
closure.

If anything fails before the VM boots, run `sprout doctor`: it checks every
prerequisite and prints the fix.

## Where to go next

- Add packages or services to the guest: every `sprout.vms.<name>` takes
  ordinary NixOS modules via `modules = [ ./guest.nix ]`; see the
  [configuration reference](../../docs/reference/configuration.md).
- Boot a second instance from another worktree of the same repo and watch
  them run side by side: `git worktree add ../feature && cd ../feature &&
  sprout up`.
