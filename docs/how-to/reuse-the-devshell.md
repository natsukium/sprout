# Reuse the project devShell in the guest

Give the guest the toolchain your devShell already declares, so
`sprout exec -- <build command>` finds the project's compilers and tools
without a `nix develop` layer inside the guest.

`inputs.sprout.lib.devShellPackages` turns a devShell into a guest package
list. It papers over the two gaps a naive `devShell.nativeBuildInputs` copy
has: multi-output packages whose `dev` output carries no binaries, and the
coreutils/C-compiler baseline a shell inherits from stdenv instead of
listing. Pass the *guest*-system `pkgs` (via flake-parts' `withSystem`), so
everything lands as Linux binaries. The guest system depends on the host the
bundle is built for (`aarch64-linux` on a Mac, the host's own system on
Linux), and `sprout.vms` is evaluated once for every host, so read it from the
guest module's own `pkgs` rather than naming it:

```nix
{ inputs, withSystem, ... }:
{
  sprout.vms.dev.modules = [
    (
      { pkgs, ... }:
      {
        environment.systemPackages = withSystem pkgs.stdenv.hostPlatform.system (
          { config, pkgs, ... }:
          inputs.sprout.lib.devShellPackages {
            devShell = config.devShells.default;
            inherit pkgs;
          }
        );
      }
    )
  ];
}
```

This requires the project flake to expose a devShell for each guest system
(`aarch64-linux`, and `x86_64-linux` for `x86_64` Linux hosts) by listing
them in `systems`. A package that does not build for Linux fails eval
there; declare it directly in `environment.systemPackages` instead.
