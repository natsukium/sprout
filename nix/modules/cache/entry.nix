{ lib, ... }:
{
  options = {
    enable = lib.mkEnableOption "this cache (built-ins are opt-in too)";
    guestPath = lib.mkOption {
      type = lib.types.str;
      description = "Where the cache is mounted inside the guest.";
    };
    scope = lib.mkOption {
      type = lib.types.enum [
        "project"
        "shared"
        "instance"
      ];
      # The narrowest sharing that still crosses branches, because `shared` is
      # writable by every project on the host: widening that is a decision an
      # entry should state, not inherit.
      default = "project";
      description = "`project` reuses one host tree across every instance of the same clone; `shared` widens that to every project on the host; `instance` drops the host share and backs the cache with the guest's own /var, removed by `sprout delete`.";
    };
    guestEnv = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      example = {
        SCCACHE_DIR = "/root/.cache/sccache";
      };
      description = "Environment variables set in the guest, so the tool that uses the cache is pointed at guestPath in the same place the cache is declared.";
    };
    guestModule = lib.mkOption {
      type = lib.types.nullOr lib.types.deferredModule;
      default = null;
      description = "NixOS module merged into the guest while this cache is enabled (the tool's package, its config).";
    };
  };
}
