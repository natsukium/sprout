{ lib }:
let
  freeformEntriesOf =
    entry:
    lib.types.submodule {
      freeformType = lib.types.attrsOf (lib.types.submodule entry);
    };

  vmModule = {
    imports = import ./modules;
    options = {
      vcpu = lib.mkOption {
        type = lib.types.int;
        default = 4;
        description = "Number of virtual CPUs.";
      };
      mem = lib.mkOption {
        type = lib.types.either lib.types.int lib.types.str;
        default = 8192;
        example = "8GiB";
        description = "Memory: an integer MiB count, or a human string like \"8GiB\"/\"512MiB\".";
      };
      diskSize = lib.mkOption {
        type = lib.types.either lib.types.int lib.types.str;
        default = 102400;
        example = "100GiB";
        description = "Persistent /var volume size (sparse): an integer MiB count, or a human string like \"100GiB\".";
      };
      workspace = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Mount the host workspace (git toplevel) at /workspace.";
      };
      writableStore = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Overlay a writable /nix/store over the host share, so `nix build` works inside the guest. Backed by the persistent /var volume.";
      };
      hostLoopback = lib.mkOption {
        type = lib.types.bool;
        # Off by default: the alias exposes every 127.0.0.1 listener on the
        # host to anything running in the guest, which undercuts "the
        # isolation boundary is the VM".
        default = false;
        description = "Let the guest reach the host's loopback interface at 192.168.127.254.";
      };
      modules = lib.mkOption {
        type = lib.types.listOf lib.types.deferredModule;
        default = [ ];
        description = "NixOS modules merged into the guest configuration.";
      };
      dns = lib.mkOption {
        type = lib.types.submodule {
          options = {
            wildcardDomains = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [ ];
              example = [ "sprout.localhost" ];
              description = "Domains whose every subdomain the embedded stack's resolver answers with 127.0.0.1, the guest's own loopback; other queries resolve as they otherwise would.";
            };
          };
        };
        default = { };
        description = "Guest-internal DNS overrides.";
      };
      credentials = lib.mkOption {
        type = freeformEntriesOf ./modules/credential/entry.nix;
        default = { };
        example = {
          aws.enable = true;
          gh.enable = true;
        };
        description = "Host credentials projected into the guest per instance; opt in per name.";
      };
      caches = lib.mkOption {
        type = freeformEntriesOf ./modules/cache/entry.nix;
        default = { };
        example = {
          sccache.enable = true;
        };
        description = "Named read-write build caches reused across instances; `scope` sets how far each reaches and whether a host share or the guest's own volume backs it. Opt in per name.";
      };
      idle = lib.mkOption {
        type = lib.types.submodule {
          options = {
            action = lib.mkOption {
              type = lib.types.enum [
                "none"
                "stop"
              ];
              # Identity is branch-bound, so instances accumulate across a
              # session and auto-stop is the only automatic reclaim of their
              # host memory.
              default = "stop";
              description = "`stop` powers the instance off after `after` of no activity (SSH sessions and routed HTTP connections); `none` disables auto-stop.";
            };
            after = lib.mkOption {
              type = lib.types.str;
              default = "2h";
              description = "Idle period before `action` fires, as a Go duration (e.g. \"30m\", \"2h\").";
            };
          };
        };
        default = { };
        example = {
          action = "stop";
          after = "2h";
        };
        description = "Auto-stop an idle instance to reclaim host memory (a full poweroff; /var survives).";
      };
    };
  };
in
{
  inherit vmModule;
  # The submodule type is a function of the host package set because built-in
  # modules render host-side scripts (gh's materialize helper) from it. The
  # argument is only forced when such a script is actually read, so passing a
  # lazy thunk (e.g. a withSystem call) is safe.
  #
  # extraModules is how a consumer that wraps a VM definition — the nix-darwin
  # module, which hangs launchd options off each instance — adds its own options
  # beside the guest's. Building a second submodule type instead would land on
  # `types.submodule`'s shorthandOnlyDefinesConfig, giving that consumer
  # different shorthand semantics from `sprout.vms.<name>` for no stated reason.
  vmTypeWith =
    hostPkgs: extraModules:
    lib.types.submoduleWith {
      modules = [
        vmModule
        { _module.args.hostPkgs = hostPkgs; }
      ]
      ++ extraModules;
    };
}
