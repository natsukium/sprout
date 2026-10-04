{ inputs, self, ... }:
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
