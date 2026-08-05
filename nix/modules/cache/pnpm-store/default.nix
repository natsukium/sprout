# pnpm only hardlinks from a store on the same filesystem as node_modules, so
# across virtiofs mounts it degrades to copy mode — sharing trades disk dedup
# for cross-instance download reuse.
{ lib, ... }:
{
  options.caches.pnpm-store = lib.mkOption {
    type = lib.types.submodule [
      ../entry.nix
      (
        { config, ... }:
        {
          guestPath = lib.mkDefault "/root/.local/share/pnpm/store";
          # Cross-project sharing is the point: the store is keyed by package
          # content, so a tarball another project already fetched is the same
          # tarball here.
          scope = lib.mkDefault "shared";
          # pnpm resolves the store from $PNPM_HOME ahead of any default path,
          # so relying on its built-in default lets an unrelated PNPM_HOME
          # strand the mount. Both prefixes are set because pnpm 11 reads only
          # pnpm_config_*, while pnpm 10 and earlier read only npm_config_*.
          guestEnv.pnpm_config_store_dir = lib.mkDefault config.guestPath;
          guestEnv.npm_config_store_dir = lib.mkDefault config.guestPath;
        }
      )
    ];
    default = { };
    description = "The pnpm content-addressable package store.";
  };
}
