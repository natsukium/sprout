{
  description = "Development inputs for sprout, loaded as the root flake's `dev` partition";

  # Declaring these inputs in the root flake would copy them into the lock
  # file of every project that consumes sprout; the partitions module
  # (`partitions.dev` in ../flake.nix) pulls them in for the dev partition only.
  inputs = {
    # Named `nixpkgs-dev` rather than `nixpkgs`: a partition's inputs are the
    # root flake's inputs overlaid with this flake's, so an input named
    # `nixpkgs` would shadow the root pin inside the partition and let the
    # devShell's Go toolchain drift from the one that builds `packages.sprout`.
    # Nothing evaluates this pin — it exists so treefmt-nix and git-hooks have
    # a single nixpkgs to follow rather than dragging in one each.
    nixpkgs-dev.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    treefmt-nix.url = "github:numtide/treefmt-nix";
    treefmt-nix.inputs.nixpkgs.follows = "nixpkgs-dev";
    git-hooks.url = "github:cachix/git-hooks.nix";
    git-hooks.inputs.nixpkgs.follows = "nixpkgs-dev";
  };

  # Only `inputs` is read; the partition supplies the outputs.
  outputs = _: { };
}
