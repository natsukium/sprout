{ inputs, ... }:
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
