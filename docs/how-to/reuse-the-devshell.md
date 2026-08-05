# Reuse the project devShell in the guest

Give the guest the toolchain your devShell already declares, so
`sprout exec -- <build command>` finds the project's compilers and tools
without a `nix develop` layer inside the guest.

`inputs.sprout.lib.devShellPackages` turns a devShell into a guest package
list. It papers over the two gaps a naive `devShell.nativeBuildInputs` copy
has: multi-output packages whose `dev` output carries no binaries, and the
coreutils/C-compiler baseline a shell inherits from stdenv instead of
listing. Pass the *guest*-system `pkgs` (via flake-parts' `withSystem`), so
everything lands as Linux binaries:

```nix
{ inputs, withSystem, ... }:
{
  sprout.vms.dev.modules = [
    {
      environment.systemPackages = withSystem "aarch64-linux" (
        { config, pkgs, ... }:
        inputs.sprout.lib.devShellPackages {
          devShell = config.devShells.default;
          inherit pkgs;
        }
      );
    }
  ];
}
```

This requires the project flake to expose an `aarch64-linux` devShell (add
the system to `systems`). A package that does not cross-build fails eval
there; declare it directly in `environment.systemPackages` instead.
