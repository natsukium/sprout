# The shared-cache contract, end to end: one host tree mounted into every
# instance that enables the cache, guestEnv landing in login shells, and the
# guestModule half installing the tool. Uses the sccache built-in because it
# exercises all three fields at once.
{ ... }:
{
  vm = {
    caches = {
      sccache.enable = true;
      pnpm-store = {
        enable = true;
        # Away from the module default, which is also pnpm's own default store
        # path: on that path a store pnpm resolved by itself is
        # indistinguishable from one it took from sprout's environment, and
        # the variable could be one pnpm never reads.
        guestPath = "/root/.cache/pnpm-store";
      };
    };
    # pnpm-store carries no guestModule, so the guest gets a pnpm to resolve
    # the store with.
    modules = [
      (
        { pkgs, ... }:
        {
          environment.systemPackages = [ pkgs.pnpm ];
        }
      )
    ];
  };
  testScript = ''
    up a
    up b
    guest a 'command -v sccache'
    guest a 'test "$SCCACHE_DIR" = /root/.cache/sccache'
    guest a 'test "$RUSTC_WRAPPER" = sccache'
    # The store pnpm actually resolves, not the variables sprout exports:
    # a variable this pnpm ignores would leave the mount unused while every
    # install still succeeds. Matched by prefix because the store-version
    # suffix moves with pnpm's major.
    guest a 'pnpm store path | grep -q "^/root/\.cache/pnpm-store/"'
    # The compatibility half for pnpm 10 and earlier, which the guest's pnpm
    # 11 ignores and so cannot resolve.
    guest a 'test "$npm_config_store_dir" = /root/.cache/pnpm-store'
    guest a 'touch /root/.cache/sccache/shared-marker'
    guest b 'test -f /root/.cache/sccache/shared-marker'
  '';
}
