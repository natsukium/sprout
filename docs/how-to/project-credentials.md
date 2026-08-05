# Project host credentials into a guest

Give a guest access to a host credential (GitHub, AWS, an OAuth token, the
ssh-agent) without copying your dotfiles wholesale. Every credential is
opt-in by name.

For the strategy semantics and the full built-in table, see the
[credentials reference](../reference/configuration.md#credentials).

## Enable a built-in

```nix
sprout.vms.dev.credentials = {
  gh.enable = true;
  aws.enable = true;
  ssh-agent.enable = true;
};
```

`sprout up` projects each credential before boot. A missing mount source or
failed `materialize` command aborts the boot.

## Lock down a writable mount

`aws` mounts `~/.aws` read-write by default so `aws sso login` on either
side refreshes the shared token cache. To stop guest processes from
modifying your host credentials, make it read-only:

```nix
sprout.vms.dev.credentials.aws = {
  enable = true;
  readOnly = true;
};
```

`readOnly` is enforced by the guest's own mount options, because virtiofs on
macOS has no host-side read-only flag. That stops accidents, not a hostile
guest: root inside the guest can remount and write. For credentials the
guest must never alter, prefer `materialize` or `socket`; see [what the
guest can reach](../explanation/architecture.md#what-the-guest-can-reach).

## Add a custom credential

Choose a strategy explicitly and point it at a source:

```nix
sprout.vms.dev.credentials.npm = {
  enable = true;
  strategy = "mount";
  source = "~/.npmrc";
  target = "/root/.npmrc";
};
```

`source` may start with `~` or `$VAR`. The binary resolves symlinks because
virtiofs cannot follow one used as the shared directory, as with sops-nix's
`/run/secrets`.
