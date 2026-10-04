{ localInputs, lib }:
let
  inherit (import ./options.nix { inherit lib; }) vmModule vmTypeWith;
  inherit (import ./bundle.nix { inherit localInputs lib; }) mkBundle mkGuest;

  # nativeBuildInputs alone omits required tools: multi-output packages surface
  # their `dev` output there, which for some (nodejs-slim) carries no bin/,
  # so each input's `out` is added alongside; and a shell inherits coreutils
  # and a C compiler from stdenv, restored here via initialPath/stdenv.cc.
  # `pkgs` must be the guest-system package set (typically reached via
  # flake-parts' withSystem) so everything lands as Linux binaries.
  devShellPackages =
    { devShell, pkgs }:
    lib.unique (
      lib.concatMap (p: [ p ] ++ lib.optional (p ? out) p.out) devShell.nativeBuildInputs
      ++ pkgs.stdenv.initialPath
      ++ [ pkgs.stdenv.cc ]
    );

  mkVM =
    {
      pkgs,
      name ? "dev",
      ...
    }@args:
    mkBundle pkgs name
      (lib.evalModules {
        modules = [
          vmModule
          { _module.args.hostPkgs = pkgs; }
          (builtins.removeAttrs args [
            "pkgs"
            "name"
          ])
        ];
      }).config;

  # Callers nest the result under sproutConfigurations.<system>, the shape
  # discovery reads.
  mkVMs = { pkgs, vms }: lib.mapAttrs (name: vmCfg: mkVM (vmCfg // { inherit pkgs name; })) vms;

  # What the sprout binary shells out to (git, ssh, and `nix build` for GC
  # roots): launchd jobs see only this PATH. The vfkit runner carries its own
  # closure and needs nothing here.
  hostTools = pkgs: [
    pkgs.git
    pkgs.openssh
    pkgs.nix
  ];
in
{
  inherit
    vmTypeWith
    vmModule
    mkBundle
    mkGuest
    mkVM
    mkVMs
    devShellPackages
    hostTools
    ;
  # Export paths so custom entries evaluate in the consumer's module system.
  cacheEntryModule = ./modules/cache/entry.nix;
  credentialEntryModule = ./modules/credential/entry.nix;
}
