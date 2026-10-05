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
  resolveHome =
    user: home: if home != null then home else config.users.users.${user}.home or "/home/${user}";

  ownerOptions = userDescription: {
    user = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = userDescription;
    };
    home = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Override that user's home directory (defaults to `users.users.<user>.home`); sets HOME, the working directory, and so where per-user state resolves.";
    };
  };

  # System units, not user units: a user manager runs only while its user is
  # logged in or lingering.
  runAs = user: home: {
    environment.HOME = home;
    unitConfig.RequiresMountsFor = [ home ];
    serviceConfig = {
      User = user;
      WorkingDirectory = home;
    };
  };

  hostTools = sproutLib.hostTools pkgs;

  sprout = "${cfg.package}/bin/sprout";

  daemon =
    name: inst:
    let
      user = resolveUser inst.user;
      home = resolveHome user inst.home;
      bundle = sproutLib.mkBundle pkgs name inst;
      supervised = cfg.autoStart && inst.autoStart;
    in
    lib.nameValuePair "sprout-${name}" (
      lib.recursiveUpdate (runAs user home) {
        description = "sprout instance ${name}";
        path = hostTools;
        wantedBy = lib.optional supervised "multi-user.target";
        # At shutdown the guest powers off before these stop: network and nix
        # daemon (GC roots) stay up, and the sweep cannot stop it from under
        # its unit. The socket alone cannot restart the nix daemon then.
        after = [
          "network.target"
          "nix-daemon.socket"
          "nix-daemon.service"
          "sprout-route-instances.service"
        ];
        serviceConfig = {
          # The default `up` exits after readiness, so systemd requires
          # --foreground.
          ExecStart = "${sprout} up --foreground --bundle ${bundle} --instance ${name}";
          Restart = if supervised then "always" else "no";
          RestartSec = 30;
          # control-group would SIGTERM QEMU too, which quits on it without the
          # guest poweroff sprout asks for.
          KillMode = "mixed";
          # Above sprout's worst case: a 30s guest poweroff, a 15s SIGTERM wait
          # and the sidecars' teardown.
          TimeoutStopSec = 90;
        };
      }
    );

  routeSocketName = "route";

  routeAddresses =
    let
      a = cfg.route.bindAddress;
    in
    if a == "localhost" then
      [ "127.0.0.1" ] ++ lib.optional config.networking.enableIPv6 "[::1]"
    else if lib.hasInfix ":" a && !lib.hasPrefix "[" a then
      [ "[${a}]" ]
    else
      [ a ];

  routeSocket = {
    description = "sprout router socket";
    wantedBy = [ "sockets.target" ];
    listenStreams = map (a: "${a}:${toString cfg.route.port}") routeAddresses;
    socketConfig.FileDescriptorName = routeSocketName;
  };

  routeService =
    let
      user = resolveUser cfg.route.user;
      home = resolveHome user cfg.route.home;
    in
    lib.recursiveUpdate (runAs user home) {
      description = "sprout router";
      # Wake re-execs `sprout start`, so it needs the daemon's host tools.
      path = hostTools;
      requires = [ "sprout-route.socket" ];
      wants = [ "sprout-route-instances.service" ];
      # After the sweep, so at shutdown the router stops waking instances
      # before the sweep stops them.
      after = [
        "sprout-route.socket"
        "sprout-route-instances.service"
        "network.target"
        "nix-daemon.socket"
        "nix-daemon.service"
      ];
      serviceConfig = {
        ExecStart =
          "${sprout} route serve --activated-socket ${routeSocketName} --domain ${cfg.route.domain}"
          + lib.optionalString (!cfg.route.wake) " --no-wake";
        Restart = "on-failure";
        RestartSec = 10;
        # Woken daemons live in this unit's cgroup; the default would stop
        # them on every router restart.
        KillMode = "process";
      };
    };

  # Woken instances have no unit of their own; without this they meet
  # systemd's final SIGTERM, which QEMU quits on without a guest poweroff.
  routeInstancesService =
    let
      user = resolveUser cfg.route.user;
      home = resolveHome user cfg.route.home;
    in
    lib.recursiveUpdate (runAs user home) {
      description = "Graceful stop of sprout instances the router woke";
      path = hostTools;
      wantedBy = [ "multi-user.target" ];
      after = [
        "network.target"
        "nix-daemon.socket"
        "nix-daemon.service"
      ];
      restartIfChanged = false;
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${pkgs.coreutils}/bin/true";
        ExecStop = "${sprout} stop --all";
        # One instance's budget, since `stop --all` is concurrent: up to 30s
        # (bootingServeWait) for a booting one, then its graceful stop.
        TimeoutStopSec = 120;
      };
    };
in
{
  options.services.sprout = {
    enable = lib.mkEnableOption "supervised sprout instances under systemd";

    package = lib.mkOption {
      type = lib.types.package;
      # Build binary and bundles from one input to prevent format skew.
      default = localInputs.self.packages.${pkgs.stdenv.hostPlatform.system}.sprout;
      defaultText = lib.literalExpression "sprout.packages.\${system}.sprout";
      description = "The sprout package whose binary the systemd units run.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      description = ''
        User the systemd units run as (their User=). Per-user state and the
        instance sockets resolve relative to this user, so it must be the one
        whose `sprout` commands manage the instances. Override per instance
        with `instances.<name>.user`.
      '';
    };

    autoStart = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Master switch for starting and restarting every instance; an instance can still opt out via its own `autoStart`.";
    };

    route = {
      enable = lib.mkEnableOption ''
        the sprout router under systemd. A socket unit binds the port as root
        and hands it to `sprout route serve` running as `user`, which is the
        only way to serve :80 without running the router itself as root or
        lowering net.ipv4.ip_unprivileged_port_start for every user — and :80
        is what lets a guest's absolute URLs stay portless
      '';

      port = lib.mkOption {
        type = lib.types.port;
        default = 80;
        description = "Host port the socket unit binds for the router.";
      };

      bindAddress = lib.mkOption {
        type = lib.types.str;
        default = "localhost";
        description = ''
          Address the socket unit binds. `localhost` binds both 127.0.0.1 and
          ::1 (the latter only with networking.enableIPv6), so it works
          whichever one the resolver hands a browser. A non-loopback address
          makes every instance's every guest port reachable by anything that
          can send a matching Host header.
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
      description = "Supervised microVM instances, each run under its own systemd service.";
      type = lib.types.attrsOf (
        sproutLib.vmTypeWith pkgs [
          {
            options =
              ownerOptions "Override the owning user for this instance (defaults to `services.sprout.user`)."
              // {
                autoStart = lib.mkOption {
                  type = lib.types.bool;
                  default = true;
                  description = "Start this instance at boot and restart it whenever it exits. Set false to define the unit without supervising it; `systemctl start sprout-<name>` then runs it once.";
                };
              };
          }
        ]
      );
    };
  };

  # The router also reaches hand-started instances, so it needs no supervised
  # one.
  config = lib.mkIf (cfg.enable && (cfg.instances != { } || cfg.route.enable)) {
    systemd.services =
      lib.mapAttrs' daemon cfg.instances
      // lib.optionalAttrs cfg.route.enable {
        sprout-route = routeService;
        sprout-route-instances = routeInstancesService;
      };
    systemd.sockets = lib.optionalAttrs cfg.route.enable {
      sprout-route = routeSocket;
    };
  };
}
