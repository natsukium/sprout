# A dedicated `sproutConfigurations` output rather than `packages`: dev VMs
# are development machinery, and listing them among a project's real packages
# clutters `nix flake show` for something nobody builds by hand. The `sprout-`
# prefix on nixosConfigurations, which exists so stock tooling can introspect a
# guest, keeps guests clear of a project's own systems.
{ localInputs }:
{
  config,
  lib,
  withSystem,
  ...
}:
let
  sproutLib = import ./lib.nix { inherit localInputs lib; };
  # Bundles are host-side artifacts and sprout only ships an aarch64-darwin
  # runner; guarding on `systems` keeps withSystem from throwing in a flake
  # that does not target that host at all.
  hostSystem = "aarch64-darwin";
in
{
  options.sprout.vms = lib.mkOption {
    # hostPkgs reaches the built-in modules as a lazy thunk: withSystem would
    # throw in a flake that does not target the host system, but it is only
    # forced when a definition reads a host-side script (gh's materialize
    # exec), which can only happen under the systems guard below.
    type = lib.types.attrsOf (sproutLib.vmTypeWith (withSystem hostSystem ({ pkgs, ... }: pkgs)) [ ]);
    default = { };
    description = "Disposable development microVM definitions.";
  };

  config.flake = lib.mkIf (config.sprout.vms != { } && lib.elem hostSystem config.systems) (
    withSystem hostSystem (
      { pkgs, ... }:
      {
        sproutConfigurations = lib.mapAttrs (
          name: vmCfg: sproutLib.mkBundle pkgs name vmCfg
        ) config.sprout.vms;
        nixosConfigurations = lib.mapAttrs' (
          name: vmCfg: lib.nameValuePair "sprout-${name}" (sproutLib.mkGuest pkgs name vmCfg)
        ) config.sprout.vms;
      }
    )
  );
}
