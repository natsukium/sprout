{
  description = "sprout example: a Postgres-backed todo app, run on k3s";

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
        vcpu = 4;
        mem = "6GiB";
        modules = [
          inputs.sprout.nixosModules.k3s
          ./guest.nix
        ];
      };
    };
}
