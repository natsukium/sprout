{
  description = "Disposable, declarative Linux microVMs for per-branch development";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    flake-parts.inputs.nixpkgs-lib.follows = "nixpkgs";
    microvm.url = "github:microvm-nix/microvm.nix";
    microvm.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } (
      { ... }:
      let
        flakeModule = flake-parts.lib.importApply ./nix/flake-module.nix {
          localInputs = inputs;
        };
        darwinModule = flake-parts.lib.importApply ./nix/darwin-module.nix {
          localInputs = inputs;
        };
        nixosModule = flake-parts.lib.importApply ./nix/nixos-module.nix {
          localInputs = inputs;
        };
      in
      {
        systems = [
          "aarch64-darwin"
          "aarch64-linux"
          "x86_64-linux"
        ];
        imports = [
          flakeModule
          inputs.flake-parts.flakeModules.partitions
        ];

        partitionedAttrs = {
          checks = "dev";
          devShells = "dev";
          formatter = "dev";
          legacyPackages = "dev";
        };
        partitions.dev = {
          extraInputsFlake = ./dev;
          module.imports = [ ./dev/flake-module.nix ];
        };

        flake.flakeModules.default = flakeModule;
        flake.darwinModules.default = darwinModule;
        flake.nixosModules.default = nixosModule;
        # Exported as paths so they evaluate against whichever nixpkgs builds
        # the guest.
        flake.nixosModules.k3s = ./nix/modules/nixos/k3s.nix;
        flake.nixosModules.podman = ./nix/modules/nixos/podman.nix;
        flake.lib = {
          inherit
            (import ./nix/lib.nix {
              localInputs = inputs;
              lib = inputs.nixpkgs.lib;
            })
            mkVM
            mkVMs
            devShellPackages
            cacheEntryModule
            credentialEntryModule
            ;
        };

        sprout.vms.dev = {
          vcpu = 2;
          mem = 2048;
          dns.wildcardDomains = [ "sprout.localhost" ];
          modules = [ ];
        };

        perSystem =
          { config, pkgs, ... }:
          let
            releaseVersion =
              let
                m = builtins.match ''.*const releaseVersion = "([^"]+)".*'' (
                  builtins.readFile ./cmd/sprout/version.go
                );
              in
              if m == null then
                throw "cmd/sprout/version.go no longer declares `const releaseVersion`"
              else
                builtins.head m;
            rev =
              if inputs.self ? rev then
                builtins.substring 0 12 inputs.self.rev
              else if inputs.self ? dirtyRev then
                "${builtins.substring 0 12 inputs.self.dirtyRev}-dirty"
              else
                "unknown";
            version = "${releaseVersion}-${rev}";
            runtimeTools =
              (import ./nix/lib.nix {
                localInputs = inputs;
                lib = inputs.nixpkgs.lib;
              }).runtimeTools
                pkgs;
          in
          {
            packages.sprout = pkgs.buildGoModule {
              pname = "sprout";
              inherit version;
              src = pkgs.lib.fileset.toSource {
                root = ./.;
                fileset = pkgs.lib.fileset.unions [
                  ./cmd
                  ./go.mod
                  ./go.sum
                ];
              };
              vendorHash = "sha256-vBpg9HbqT/FSf1BqSy5I0j5VKsB5z+OXqVN6OO1hKw0=";
              subPackages = [ "cmd/sprout" ];
              ldflags = [
                "-X main.version=${version}"
              ]
              # `sprout doctor` probes the host with a virtiofsd before any bundle
              # exists; a bundle carries and boots its own.
              ++ pkgs.lib.optional pkgs.stdenv.hostPlatform.isLinux "-X main.virtiofsdPath=${pkgs.virtiofsd}/bin/virtiofsd";
              nativeBuildInputs = [ pkgs.makeBinaryWrapper ];
              postInstall = ''
                wrapProgram $out/bin/sprout \
                  --prefix PATH : ${pkgs.lib.makeBinPath runtimeTools.preferred} \
                  --suffix PATH : ${pkgs.lib.makeBinPath runtimeTools.fallback}
              '';
              doCheck = false;
              meta = {
                description = "Disposable, declarative Linux microVMs for per-branch development";
                mainProgram = "sprout";
                license = pkgs.lib.licenses.asl20;
              };
            };
            packages.default = config.packages.sprout;

            legacyPackages.sproutTests = import ./nix/tests {
              localInputs = inputs;
              lib = inputs.nixpkgs.lib;
              inherit pkgs;
              sprout = config.packages.sprout;
            };
          };
      }
    );
}
