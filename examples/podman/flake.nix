{
  description = "sprout example: a Postgres-backed todo app, run with podman";

  # In-repo path so the example works from a checkout. When copying this out
  # of the sprout tree, replace it with the published flake:
  #   inputs.sprout.url = "github:natsukium/sprout";
  inputs.sprout.url = "path:../..";
  inputs.nixpkgs.follows = "sprout/nixpkgs";
  inputs.flake-parts.follows = "sprout/flake-parts";

  outputs =
    inputs@{ flake-parts, ... }:
    flake-parts.lib.mkFlake { inherit inputs; } {
      systems = [ "aarch64-darwin" ];
      imports = [ inputs.sprout.flakeModules.default ];

      sprout.vms.todo = {
        vcpu = 2;
        mem = "2GiB";
        modules = [
          inputs.sprout.nixosModules.podman
          ./guest.nix
        ];
      };
    };
}
