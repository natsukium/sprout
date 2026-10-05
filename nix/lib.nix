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

  # What sprout itself shells out to, prepended to its PATH by the package: Nix
  # reads a git+file flake with whatever `git` comes first, which on a Mac
  # without the Command Line Tools is a stub that only prompts to install them.
  # systemd's default PATH also lacks the `ps` the orphan reaper runs.
  runtimeTools =
    pkgs:
    [
      pkgs.git
      pkgs.openssh
    ]
    ++ lib.optional pkgs.stdenv.hostPlatform.isLinux pkgs.procps;

  # Nix is left to the service PATH rather than the package's: prepending it
  # would override the version the user installed.
  hostTools = pkgs: runtimeTools pkgs ++ [ pkgs.nix ];
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
    runtimeTools
    ;
  # Export paths so custom entries evaluate in the consumer's module system.
  cacheEntryModule = ./modules/cache/entry.nix;
  credentialEntryModule = ./modules/credential/entry.nix;
}
