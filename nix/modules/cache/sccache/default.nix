{ lib, ... }:
{
  options.caches.sccache = lib.mkOption {
    # lib.types.submodule rather than submoduleWith: submodule hard-codes
    # shorthandOnlyDefinesConfig = true, and the flag must agree wherever a
    # built-in's entry type meets the freeform base type, or type merging
    # fails with "conflicting shorthandOnlyDefinesConfig values".
    type = lib.types.submodule [
      ../entry.nix
      (
        { config, ... }:
        {
          options.maxSize = lib.mkOption {
            type = lib.types.nullOr lib.types.str;
            default = null;
            example = "20G";
            description = "On-disk size cap (sccache's SCCACHE_CACHE_SIZE); null keeps sccache's own default.";
          };
          config = {
            guestPath = lib.mkDefault "/root/.cache/sccache";
            # Widened past the default project scope: results are keyed by
            # compiler inputs, so another project's hit is a valid hit here.
            scope = lib.mkDefault "shared";
            guestEnv = lib.mkMerge [
              {
                RUSTC_WRAPPER = lib.mkDefault "sccache";
                # Follows guestPath so overriding the mount point cannot
                # silently detach the tool from its cache.
                SCCACHE_DIR = lib.mkDefault config.guestPath;
              }
              (lib.mkIf (config.maxSize != null) { SCCACHE_CACHE_SIZE = config.maxSize; })
            ];
            guestModule = lib.mkDefault (
              { pkgs, ... }:
              {
                environment.systemPackages = [ pkgs.sccache ];
              }
            );
          };
        }
      )
    ];
    default = { };
    description = "The sccache compilation cache, with the guest-side sccache install and rustc wiring.";
  };
}
