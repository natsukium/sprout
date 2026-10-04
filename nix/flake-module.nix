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
  systems = import ./systems.nix;
  targets = lib.filter (s: lib.elem s config.systems) systems.hosts;
  # Host-side credential scripts render for this one system in every host's
  # bundle, since sprout.vms is evaluated once.
  thunkSystem = if targets == [ ] then null else builtins.head targets;
  pkgsFor = hostSystem: withSystem hostSystem ({ pkgs, ... }: pkgs);
in
{
  options.sprout.vms = lib.mkOption {
    # hostPkgs reaches the built-in modules as a lazy thunk: withSystem would
    # throw in a flake that does not target the host system, but it is only
    # forced when a definition reads a host-side script (gh's materialize
    # exec), which can only happen for a targeted host below.
    type = lib.types.attrsOf (
      sproutLib.vmTypeWith (if thunkSystem == null then { } else (pkgsFor thunkSystem)) [ ]
    );
    default = { };
    description = "Disposable development microVM definitions.";
  };

  config.flake = lib.mkIf (config.sprout.vms != { }) {
    sproutConfigurations = lib.genAttrs targets (
      hostSystem:
      lib.mapAttrs (name: vmCfg: sproutLib.mkBundle (pkgsFor hostSystem) name vmCfg) config.sprout.vms
    );
    nixosConfigurations = lib.mkMerge (
      map (
        hostSystem:
        lib.mapAttrs' (
          name: vmCfg:
          lib.nameValuePair "sprout-${hostSystem}-${name}" (sproutLib.mkGuest (pkgsFor hostSystem) name vmCfg)
        ) config.sprout.vms
      ) targets
    );
  };
}
