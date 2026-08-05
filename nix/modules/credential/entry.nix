{ lib, ... }:
{
  options = {
    enable = lib.mkEnableOption "this credential (built-ins are opt-in too)";
    strategy = lib.mkOption {
      type = lib.types.nullOr (
        lib.types.enum [
          "mount"
          "materialize"
          "socket"
        ]
      );
      default = null;
      description = "How the credential reaches the guest; a built-in's module supplies the default.";
    };
    source = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Host path for `mount`, or unix socket for `socket` (may start with ~ or $VAR; Go resolves it).";
    };
    target = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Guest mount point for `mount`.";
    };
    readOnly = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Mount the credential read-only in the guest (a built-in may default this to false).";
    };
    exec = lib.mkOption {
      type = lib.types.nullOr (lib.types.listOf lib.types.str);
      default = null;
      description = "Host command for `materialize`; its stdout becomes the guest credential file.";
    };
    guestPath = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Guest path the materialized file / forwarded socket appears at.";
    };
    guestPort = lib.mkOption {
      type = lib.types.nullOr lib.types.int;
      default = null;
      description = "Gateway port the guest reaches for a `socket` forward.";
    };
    guestEnv = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      description = "Environment variables set in the guest (e.g. SSH_AUTH_SOCK for `socket`).";
    };
    guestModule = lib.mkOption {
      type = lib.types.nullOr lib.types.deferredModule;
      default = null;
      description = "NixOS module merged into the guest while this credential is enabled (the tool's package, its config).";
    };
  };
}
