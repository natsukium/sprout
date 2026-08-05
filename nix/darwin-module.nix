# A prebuilt bundle bypasses flake evaluation at boot.
{ localInputs }:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  sproutLib = import ./lib.nix { inherit localInputs lib; };
  cfg = config.services.sprout;

  resolveUser = user: if user != null then user else cfg.user;
  resolveHome = user: home: if home != null then home else "/Users/${user}";

  ownerOptions = userDescription: {
    user = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = userDescription;
    };
    home = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Override that user's home directory (defaults to /Users/<user>); sets HOME and the log location.";
    };
  };

  # LaunchAgents die at logout, so use LaunchDaemons but drop each job to its
  # owner for per-user state, Keychain access, and $SSH_AUTH_SOCK.
  runAs = user: home: logName: {
    UserName = user;
    WorkingDirectory = home;
    EnvironmentVariables = {
      HOME = home;
    };
    StandardOutPath = "${home}/Library/Logs/sprout/${logName}.out.log";
    StandardErrorPath = "${home}/Library/Logs/sprout/${logName}.err.log";
  };

  hostTools = sproutLib.hostTools pkgs;

  daemon =
    name: inst:
    let
      user = resolveUser inst.user;
      home = resolveHome user inst.home;
      bundle = sproutLib.mkBundle pkgs name inst;
    in
    lib.nameValuePair "sprout-${name}" {
      path = hostTools;
      # The default `up` exits after readiness, so launchd requires --foreground.
      command = "${cfg.package}/bin/sprout up --foreground --bundle ${bundle} --instance ${name}";
      serviceConfig = {
        KeepAlive = cfg.autoStart && inst.autoStart;
        ThrottleInterval = 30;
        # launchd sends SIGTERM on stop and sprout powers the VM off gracefully
        # (gracefulStop in up.go); a clean guest poweroff plus vfkit XPC
        # teardown takes longer than the 20s launchd default before SIGKILL.
        ExitTimeOut = 60;
      }
      // runAs user home name;
    };

  routeSocketName = "Listeners";

  # launchd must bind loopback :80 as root, but the router runs as the user to
  # read per-user state.
  routeDaemon =
    let
      user = resolveUser cfg.route.user;
      home = resolveHome user cfg.route.home;
    in
    {
      # Wake re-execs `sprout start`, so it needs the daemon's host tools.
      path = hostTools;
      command =
        "${cfg.package}/bin/sprout route serve --launchd-socket ${routeSocketName} --domain ${cfg.route.domain}"
        + lib.optionalString (!cfg.route.wake) " --no-wake";
      serviceConfig = {
        Sockets.${routeSocketName} = {
          SockType = "stream";
          SockProtocol = "TCP";
          SockNodeName = cfg.route.bindAddress;
          SockServiceName = toString cfg.route.port;
        };
        ThrottleInterval = 10;
      }
      // runAs user home "route";
    };
in
{
  options.services.sprout = {
    enable = lib.mkEnableOption "supervised sprout instances under launchd";

    package = lib.mkOption {
      type = lib.types.package;
      # Build binary and bundles from one input to prevent format skew.
      default = localInputs.self.packages.${pkgs.stdenv.hostPlatform.system}.sprout;
      defaultText = lib.literalExpression "sprout.packages.\${system}.sprout";
      description = "The sprout package whose binary the launchd job runs.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      description = ''
        macOS user the launchd job runs as (its UserName). Per-user state, the
        login Keychain, and $SSH_AUTH_SOCK resolve relative to this user, so it
        must be the human whose credentials the guests project. Override per
        instance with `instances.<name>.user`.
      '';
    };

    autoStart = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Master switch for KeepAlive across all instances; an instance can still opt out via its own `autoStart`.";
    };

    route = {
      enable = lib.mkEnableOption ''
        the sprout router under launchd. launchd binds the port as root and hands
        the socket to `sprout route serve` running as `user`, which is the only way to
        serve loopback :80 without running the router itself as root — and :80
        is what lets a guest's absolute URLs stay portless
      '';

      port = lib.mkOption {
        type = lib.types.port;
        default = 80;
        description = "Host port launchd binds for the router.";
      };

      bindAddress = lib.mkOption {
        type = lib.types.str;
        default = "localhost";
        description = ''
          Address launchd binds (the socket's SockNodeName). The default resolves
          to both loopback families, so it works whichever one the resolver hands
          a browser. A non-loopback address makes every instance's every guest
          port reachable by anything that can send a matching Host header.
        '';
      };

      domain = lib.mkOption {
        type = lib.types.str;
        default = "sprout.localhost";
        description = "Hostname suffix the router answers for (<name>.<domain>).";
      };

      wake = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Start a stopped instance when a request arrives for it.";
      };
    }
    // ownerOptions "Override the user the router runs as (defaults to `services.sprout.user`). It must be the user whose instances it routes to, since it reads their state directory.";

    instances = lib.mkOption {
      default = { };
      description = "Supervised microVM instances, each run under its own launchd job.";
      type = lib.types.attrsOf (
        sproutLib.vmTypeWith pkgs [
          {
            options =
              ownerOptions "Override the owning user for this instance (defaults to `services.sprout.user`)."
              // {
                autoStart = lib.mkOption {
                  type = lib.types.bool;
                  default = true;
                  description = "Keep this instance running (launchd KeepAlive). Set false to define it without supervising it.";
                };
              };
          }
        ]
      );
    };
  };

  # The router is useful on its own — it reaches instances started by hand,
  # which is the common case — so it must not require any supervised instance.
  config = lib.mkIf (cfg.enable && (cfg.instances != { } || cfg.route.enable)) {
    launchd.daemons =
      lib.mapAttrs' daemon cfg.instances
      // lib.optionalAttrs cfg.route.enable {
        sprout-route = routeDaemon;
      };
  };
}
