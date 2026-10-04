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

      checks.qemu-backend =
        let
          hostPkgs = host: withSystem host ({ pkgs, ... }: pkgs);
          vmOn = host: args: self.lib.mkVM ({ pkgs = hostPkgs host; } // args);
          guestOn = host: args: (vmOn host args).nixos.config;
          refusals =
            cfg:
            lib.filter (lib.hasPrefix "sprout:") (
              map (a: a.message) (lib.filter (a: !a.assertion) cfg.assertions)
            );
          refusedFor = needle: cfg: builtins.any (lib.hasInfix needle) (refusals cfg);
          withModule = host: module: guestOn host { modules = [ module ]; };
          memOn = host: mem: (guestOn host { inherit mem; }).microvm.mem;
          nic =
            host:
            let
              m = (vmOn host { }).manifest;
            in
            {
              args = (guestOn host { }).microvm.qemu.extraArgs;
              netSubstituted = builtins.elem {
                placeholder = "/sprout/placeholder/sock/${m.backend.network.socket}";
                value = "socket:${m.backend.network.socket}";
              } m.substitutions;
            };
          expectedNic = mac: {
            args = [
              "-netdev"
              "stream,id=sprout0,server=off,addr.type=unix,addr.path=/sprout/placeholder/sock/net.sock"
              "-device"
              "virtio-net-pci,netdev=sprout0,mac=${mac},romfile="
            ];
            netSubstituted = true;
          };
          sizes = [
            512
            2047
            2048
            "2048MiB"
            "2GiB"
            2049
            4096
            "8GiB"
          ];
          failures = lib.runTests {
            testX86_64RunnerDialsTheNetSocketWithTheLeaseMac = {
              expr = nic "x86_64-linux";
              expected = expectedNic (vmOn "x86_64-linux" { }).manifest.guest.mac;
            };
            testAarch64RunnerDialsTheNetSocketWithTheLeaseMac = {
              expr = nic "aarch64-linux";
              expected = expectedNic (vmOn "aarch64-linux" { }).manifest.guest.mac;
            };
            testX86_64MicrovmNeverBootsWithExactly2048MiB = {
              expr = map (memOn "x86_64-linux") sizes;
              expected = [
                512
                2047
                2050
                2050
                2050
                2049
                4096
                8192
              ];
            };
            testAarch64VirtKeepsTheRequestedMemory = {
              expr = map (memOn "aarch64-linux") sizes;
              expected = [
                512
                2047
                2048
                2048
                2048
                2049
                4096
                8192
              ];
            };
            testDefaultLinuxGuestsPassEveryQemuCheck = {
              expr = map (host: refusals (guestOn host { })) [
                "x86_64-linux"
                "aarch64-linux"
              ];
              expected = [
                [ ]
                [ ]
              ];
            };
            testForcing2048MiBOnMicrovmIsRefused = {
              expr = refusedFor "exactly 2048 MiB" (
                withModule "x86_64-linux" { microvm.mem = lib.mkForce 2048; }
              );
              expected = true;
            };
            testQemuWithoutStreamNetdevIsRefused = {
              expr = refusedFor "QEMU 7.2 or newer" (
                withModule "x86_64-linux" (
                  { pkgs, ... }:
                  {
                    microvm.qemu.package = lib.mkForce (pkgs.qemu_kvm // { version = "7.1.0"; });
                  }
                )
              );
              expected = true;
            };
            testMemoryOrMachineInExtraArgsIsRefused = {
              expr =
                map
                  (
                    arg:
                    refusedFor "not microvm.qemu.extraArgs" (
                      withModule "x86_64-linux" {
                        microvm.qemu.extraArgs = [
                          arg
                          "x"
                        ];
                      }
                    )
                  )
                  [
                    "-m"
                    "--m"
                    "-M"
                    "--M"
                    "-machine"
                    "--machine"
                  ];
              expected = [
                true
                true
                true
                true
                true
                true
              ];
            };
          };
        in
        if failures == [ ] then
          pkgs.runCommand "qemu-backend" { } "touch $out"
        else
          throw "qemu backend: ${builtins.toJSON failures}";

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
