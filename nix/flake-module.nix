# A dedicated `sproutConfigurations` output rather than `packages`: dev VMs
# are development machinery, and listing them among a project's real packages
# clutters `nix flake show` for something nobody builds by hand. The `sprout-`
# prefix on nixosConfigurations, which exists so stock tooling can introspect a
# guest, keeps guests clear of a project's own systems.
{ localInputs }:
{
  config,
  options,
  lib,
  withSystem,
  ...
}:
let
  sproutLib = import ./lib.nix { inherit localInputs lib; };
  systems = import ./systems.nix;
  targets = lib.filter (s: lib.elem s config.systems) systems.hosts;
  pkgsFor = hostSystem: withSystem hostSystem ({ pkgs, ... }: pkgs);
  # The definitions are merged again per host rather than read from
  # config.sprout.vms: built-in credentials render host-side scripts from
  # hostPkgs, and a single shared merge would record one host's scripts in
  # every host's bundle.
  vmsByHost = lib.genAttrs targets (
    hostSystem:
    (lib.mergeDefinitions [ "sprout" "vms" ] (lib.types.attrsOf (
      sproutLib.vmTypeWith (pkgsFor hostSystem) [ ]
    )) options.sprout.vms.definitionsWithLocations).mergedValue
  );
in
{
  options.sprout.vms = lib.mkOption {
    type = lib.types.attrsOf (
      sproutLib.vmTypeWith
        (throw "sprout: config.sprout.vms has no host; read host-side values from sproutConfigurations.<system>.<name>.manifest")
        [ ]
    );
    default = { };
    description = "Disposable development microVM definitions.";
  };

  config.flake = lib.mkIf (config.sprout.vms != { }) {
    sproutConfigurations = lib.genAttrs targets (
      hostSystem:
      lib.mapAttrs (
        name: vmCfg: sproutLib.mkBundle (pkgsFor hostSystem) name vmCfg
      ) vmsByHost.${hostSystem}
    );
    nixosConfigurations = lib.mkMerge (
      map (
        hostSystem:
        lib.mapAttrs' (
          name: vmCfg:
          lib.nameValuePair "sprout-${hostSystem}-${name}" (sproutLib.mkGuest (pkgsFor hostSystem) name vmCfg)
        ) vmsByHost.${hostSystem}
      ) targets
    );
  };
}
