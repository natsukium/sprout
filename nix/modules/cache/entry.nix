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
    owner = lib.mkOption {
      type = lib.types.nullOr (
        lib.types.submodule {
          options = {
            uid = lib.mkOption {
              type = lib.types.ints.unsigned;
              description = "Guest uid the cache belongs to.";
            };
            gid = lib.mkOption {
              type = lib.types.ints.unsigned;
              description = "Guest gid the cache belongs to.";
            };
          };
        }
      );
      default = null;
      example = {
        uid = 65532;
        gid = 65532;
      };
      description = "Guest user and group the cache belongs to, for a tool that does not run as root (a distroless pod's 65532); `null` leaves it to root. Everything in the cache belongs to that owner, so it is meant for one uid, not several; a host-backed cache on vfkit ignores it.";
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
