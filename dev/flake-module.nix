{
  inputs,
  self,
  lib,
  withSystem,
  ...
}:
let
  credentialHostCheck = import ./credential-host-check.nix { inherit inputs lib; };
in
{
  imports = [
    inputs.treefmt-nix.flakeModule
    inputs.git-hooks.flakeModule
  ];

  perSystem =
    {
      config,
      lib,
      pkgs,
      ...
    }:
    {
      treefmt = {
        projectRootFile = "flake.nix";
        programs.nixfmt.enable = true;
        programs.gofmt.enable = true;
        # nix/guest/worktree-git.sh runs inside the guest's minimal shell, so it is
        # POSIX sh rather than bash.
        programs.shfmt.enable = true;
      };

      pre-commit.settings = {
        package = pkgs.prek;
        hooks = {
          treefmt.enable = true;
          check-merge-conflicts.enable = true;
        };
      };

      # `nix flake check` only evaluates `packages`, so a stale vendorHash or a
      # host-specific build break would otherwise pass.
      checks.sprout = config.packages.sprout;
      checks.credential-host = credentialHostCheck pkgs;

      checks.guest-host-gating =
        let
          guestOn = host: self.nixosConfigurations."sprout-${host}-dev".config;
          apfsRepair = cfg: {
            unit = cfg.systemd.units ? "fix-iptables-case-hack.service";
            loginEnv = cfg.environment.variables ? XTABLES_LIBDIR;
            unitEnv = cfg.systemd.globalEnvironment ? XTABLES_LIBDIR;
          };
          present = {
            unit = true;
            loginEnv = true;
            unitEnv = true;
          };
          absent = lib.mapAttrs (_: _: false) present;
          failures = lib.runTests {
            testDarwinHostedGuestRepairsCaseHackedXtables = {
              expr = apfsRepair (guestOn "aarch64-darwin");
              expected = present;
            };
            testAarch64LinuxHostedGuestOmitsApfsRepair = {
              expr = apfsRepair (guestOn "aarch64-linux");
              expected = absent;
            };
            testX86_64LinuxHostedGuestOmitsApfsRepair = {
              expr = apfsRepair (guestOn "x86_64-linux");
              expected = absent;
            };
          };
        in
        if failures == [ ] then
          pkgs.runCommand "guest-host-gating" { } "touch $out"
        else
          throw "guest host gating: ${builtins.toJSON failures}";

      checks.backend-selection =
        let
          hostPkgs = host: withSystem host ({ pkgs, ... }: pkgs);
          manifestOf =
            host: backend:
            (self.lib.mkVM {
              pkgs = hostPkgs host;
              inherit backend;
            }).manifest;
          contractOf = host: backend: {
            inherit (manifestOf host backend) version host;
            kind = (manifestOf host backend).backend.kind;
            guest = (manifestOf host backend).guest.system;
          };
          refused =
            host: backend: !(builtins.tryEval (builtins.deepSeq (manifestOf host backend) true)).success;
          failures = lib.runTests {
            testAutoResolvesToVfkitOnDarwin = {
              expr = contractOf "aarch64-darwin" "auto";
              expected = {
                version = 2;
                host.system = "aarch64-darwin";
                kind = "vfkit";
                guest = "aarch64-linux";
              };
            };
            testAutoResolvesToQemuOnAarch64Linux = {
              expr = contractOf "aarch64-linux" "auto";
              expected = {
                version = 2;
                host.system = "aarch64-linux";
                kind = "qemu";
                guest = "aarch64-linux";
              };
            };
            testAutoResolvesToQemuOnX86_64Linux = {
              expr = contractOf "x86_64-linux" "auto";
              expected = {
                version = 2;
                host.system = "x86_64-linux";
                kind = "qemu";
                guest = "x86_64-linux";
              };
            };
            testExplicitAllowedKindIsKept = {
              expr = (contractOf "aarch64-darwin" "vfkit").kind;
              expected = "vfkit";
            };
            testQemuOnDarwinFailsEvaluation = {
              expr = refused "aarch64-darwin" "qemu";
              expected = true;
            };
            testVfkitOnLinuxFailsEvaluation = {
              expr = refused "x86_64-linux" "vfkit";
              expected = true;
            };
            testDefaultIsAuto = {
              expr =
                (contractOf "x86_64-linux" "auto") == {
                  inherit (self.sproutConfigurations.x86_64-linux.dev.manifest) version host;
                  kind = self.sproutConfigurations.x86_64-linux.dev.manifest.backend.kind;
                  guest = self.sproutConfigurations.x86_64-linux.dev.manifest.guest.system;
                };
              expected = true;
            };
          };
        in
        if failures == [ ] then
          pkgs.runCommand "backend-selection" { } "touch $out"
        else
          throw "backend selection: ${builtins.toJSON failures}";

      devShells.default = pkgs.mkShell {
        packages = [
          pkgs.git-cliff
          pkgs.go
          pkgs.gopls
          pkgs.prek
        ]
        # fish: a guest root login shell that does not quote like sh.
        ++ lib.optionals pkgs.stdenv.hostPlatform.isLinux [
          pkgs.git
          pkgs.fish
        ];
        shellHook = config.pre-commit.installationScript;
      };
    };
}
