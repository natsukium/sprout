# The registry holds immutable published crates, which would make it a natural
# shared cache, but cargo guards it with a lock at $CARGO_HOME/.package-cache,
# outside the mounted registry/ subtree — sharing the tree without the lock
# that orders writes to it. Instance scope also keeps its many small extracted
# crate sources off virtiofs, where they read slowest.
{ lib, ... }:
{
  options.caches.cargo-registry = lib.mkOption {
    type = lib.types.submodule [
      ../entry.nix
      {
        guestPath = lib.mkDefault "/root/.cargo/registry";
        scope = lib.mkDefault "instance";
      }
    ];
    default = { };
    description = "The cargo registry cache (downloaded crates).";
  };
}
