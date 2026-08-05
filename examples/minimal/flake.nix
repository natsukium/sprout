{
  description = "sprout example: the smallest configuration that boots";

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

      # A single definition lets `sprout up` omit --vm.
      sprout.vms.dev = { };
    };
}
