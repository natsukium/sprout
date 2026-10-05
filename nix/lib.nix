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

  # What sprout itself shells out to, added to its PATH by the package. git goes
  # first: Nix reads a git+file flake with whatever `git` comes first, which on a
  # Mac without the Command Line Tools is a stub that only prompts to install
  # them. The rest only fill a gap: the user's ssh reads their ~/.ssh/config,
  # whose Apple-only options (UseKeychain) Nix's OpenSSH rejects, and systemd's
  # default PATH lacks the `ps` the orphan reaper runs.
  runtimeTools = pkgs: {
    preferred = [ pkgs.git ];
    fallback = [ pkgs.openssh ] ++ lib.optional pkgs.stdenv.hostPlatform.isLinux pkgs.procps;
  };

  # Nix is left to the service PATH rather than the package's, which would
  # override the version the user installed.
  hostTools =
    pkgs:
    let
      t = runtimeTools pkgs;
    in
    t.preferred ++ t.fallback ++ [ pkgs.nix ];
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
