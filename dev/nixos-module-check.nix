# Bundles are instantiated, never built, so this runs on any checker.
{ inputs, lib }:
pkgs:
let
  hostWith = system: services: hostWithModule system services { };
  hostWithModule =
    system: services: extra:
    (inputs.nixpkgs.lib.nixosSystem {
      modules = [
        inputs.self.nixosModules.default
        extra
        {
          nixpkgs.hostPlatform = system;
          boot.loader.grub.enable = false;
          fileSystems."/" = {
            device = "none";
            fsType = "tmpfs";
          };
          system.stateVersion = lib.trivial.release;
          users.users.alice.isNormalUser = true;
          users.users.bob = {
            isNormalUser = true;
            home = "/srv/bob";
          };
          services.sprout = {
            enable = true;
            user = "alice";
          }
          // services;
        }
      ];
    }).config;
  host = hostWith "x86_64-linux";

  owner = unit: {
    inherit (unit.serviceConfig) User WorkingDirectory;
    inherit (unit.environment) HOME;
    mounts = unit.unitConfig.RequiresMountsFor;
  };

  pathHas =
    unit: tools:
    let
      dirs = lib.splitString ":" unit.environment.PATH;
    in
    map (t: builtins.elem "${t}/bin" dirs) tools;
  toolsOn = system: [
    inputs.nixpkgs.legacyPackages.${system}.git
    inputs.nixpkgs.legacyPackages.${system}.openssh
    inputs.nixpkgs.legacyPackages.${system}.nix
    inputs.nixpkgs.legacyPackages.${system}.procps
  ];

  supervision = unit: {
    inherit (unit) wantedBy;
    inherit (unit.serviceConfig) Restart;
  };

  runner = (host { instances.runner = { }; }).systemd.services.sprout-runner;
  routed = host { route.enable = true; };
  route = routed.systemd.services.sprout-route;
  sweep = routed.systemd.services.sprout-route-instances;
  routeSocket = routed.systemd.sockets.sprout-route;
  socketArg = lib.last (lib.splitString "--activated-socket " route.serviceConfig.ExecStart);

  failures = lib.runTests {
    testInstanceRunsAsTheConfiguredUser = {
      expr = owner runner;
      expected = {
        User = "alice";
        WorkingDirectory = "/home/alice";
        HOME = "/home/alice";
        mounts = [ "/home/alice" ];
      };
    };
    testInstanceOwnerFollowsThatUsersHome = {
      expr = owner (host { instances.runner.user = "bob"; }).systemd.services.sprout-runner;
      expected = {
        User = "bob";
        WorkingDirectory = "/srv/bob";
        HOME = "/srv/bob";
        mounts = [ "/srv/bob" ];
      };
    };
    testInstanceBootsItsBundleInTheForeground = {
      expr =
        builtins.match "/nix/store/[^ ]+/bin/sprout up --foreground --bundle /nix/store/[a-z0-9]+-sprout-vm-runner --instance runner" runner.serviceConfig.ExecStart
        != null;
      expected = true;
    };
    testStopSignalsOnlySproutAndWaitsOutAGuestPoweroff = {
      expr = {
        inherit (runner.serviceConfig) KillMode;
        outlastsSproutsOwnBudget = runner.serviceConfig.TimeoutStopSec >= 60;
      };
      expected = {
        KillMode = "mixed";
        outlastsSproutsOwnBudget = true;
      };
    };
    testShutdownStopsInstancesBeforeTheirDependencies = {
      expr = map (u: builtins.elem u runner.after) [
        "network.target"
        "nix-daemon.socket"
        "nix-daemon.service"
      ];
      expected = [
        true
        true
        true
      ];
    };
    testSupervisedInstanceStartsAtBootAndRestarts = {
      expr = supervision runner;
      expected = {
        wantedBy = [ "multi-user.target" ];
        Restart = "always";
      };
    };
    testUnsupervisedInstanceIsOnlyDefined = {
      expr = map supervision [
        (host { instances.runner.autoStart = false; }).systemd.services.sprout-runner
        (host {
          autoStart = false;
          instances.runner = { };
        }).systemd.services.sprout-runner
      ];
      expected = [
        {
          wantedBy = [ ];
          Restart = "no";
        }
        {
          wantedBy = [ ];
          Restart = "no";
        }
      ];
    };
    testInstanceSeesTheHostToolsOnX86_64 = {
      expr = pathHas runner (toolsOn "x86_64-linux");
      expected = [
        true
        true
        true
        true
      ];
    };
    testInstanceSeesTheHostToolsOnAarch64 = {
      expr =
        pathHas
          (hostWith "aarch64-linux" {
            instances.runner = { };
          }).systemd.services.sprout-runner
          (toolsOn "aarch64-linux");
      expected = [
        true
        true
        true
        true
      ];
    };
    testRouterSeesTheHostToolsItWakesInstancesWith = {
      expr = pathHas route (toolsOn "x86_64-linux");
      expected = [
        true
        true
        true
        true
      ];
    };
    testRouterRunsAsTheUserWhoseInstancesItRoutes = {
      expr = owner route;
      expected = {
        User = "alice";
        WorkingDirectory = "/home/alice";
        HOME = "/home/alice";
        mounts = [ "/home/alice" ];
      };
    };
    testRouterServesTheSocketSystemdBinds = {
      expr = {
        listen = routeSocket.listenStreams;
        inherit (routeSocket) wantedBy;
        namesWhatTheRouterAdopts =
          routeSocket.socketConfig.FileDescriptorName == lib.head (lib.splitString " " socketArg);
        routerRequiresTheSocket = builtins.elem "sprout-route.socket" route.requires;
        routerStartsOnlyOnDemand = route.wantedBy;
      };
      expected = {
        listen = [
          "127.0.0.1:80"
          "[::1]:80"
        ];
        wantedBy = [ "sockets.target" ];
        namesWhatTheRouterAdopts = true;
        routerRequiresTheSocket = true;
        routerStartsOnlyOnDemand = [ ];
      };
    };
    testRouterBindAddressMapsToListenStreams = {
      expr =
        map
          (
            {
              route,
              extra ? { },
            }:
            (hostWithModule "x86_64-linux" {
              route = route // {
                enable = true;
              };
            } extra).systemd.sockets.sprout-route.listenStreams
          )
          [
            {
              route = {
                bindAddress = "::";
                port = 8080;
              };
            }
            { route.bindAddress = "192.168.1.10"; }
            { route.bindAddress = "[fd00::1]"; }
            {
              route = { };
              extra.networking.enableIPv6 = false;
            }
          ];
      expected = [
        [ "[::]:8080" ]
        [ "192.168.1.10:80" ]
        [ "[fd00::1]:80" ]
        [ "127.0.0.1:80" ]
      ];
    };
    testRouterRestartLeavesWokenInstancesRunning = {
      expr = route.serviceConfig.KillMode;
      expected = "process";
    };
    testWokenInstancesGetAGracefulStopAtShutdown = {
      expr = {
        inherit (sweep) wantedBy restartIfChanged;
        inherit (sweep.serviceConfig)
          User
          Type
          RemainAfterExit
          TimeoutStopSec
          ;
        stopsEveryInstance = lib.hasSuffix "/bin/sprout stop --all" sweep.serviceConfig.ExecStop;
        stopsBeforeNetworkAndNix = map (u: builtins.elem u sweep.after) [
          "network.target"
          "nix-daemon.socket"
          "nix-daemon.service"
        ];
        routerStopsWakingFirst = builtins.elem "sprout-route-instances.service" route.after;
        routerStopsBeforeNix = builtins.elem "nix-daemon.service" route.after;
        routerPullsItIn = route.wants;
        supervisedInstancesStopThemselvesFirst = builtins.elem "sprout-route-instances.service" runner.after;
        hostTools = pathHas sweep (toolsOn "x86_64-linux");
      };
      expected = {
        wantedBy = [ "multi-user.target" ];
        restartIfChanged = false;
        User = "alice";
        Type = "oneshot";
        RemainAfterExit = true;
        TimeoutStopSec = 120;
        stopsEveryInstance = true;
        stopsBeforeNetworkAndNix = [
          true
          true
          true
        ];
        routerStopsWakingFirst = true;
        routerStopsBeforeNix = true;
        routerPullsItIn = [ "sprout-route-instances.service" ];
        supervisedInstancesStopThemselvesFirst = true;
        hostTools = [
          true
          true
          true
          true
        ];
      };
    };
    testRouterFlagsFollowTheOptions = {
      expr =
        map
          (
            r:
            lib.hasSuffix "--domain example.test --no-wake"
              (host {
                route = r // {
                  enable = true;
                };
              }).systemd.services.sprout-route.serviceConfig.ExecStart
          )
          [
            {
              domain = "example.test";
              wake = false;
            }
            { domain = "example.test"; }
          ];
      expected = [
        true
        false
      ];
    };
    testRouterNeedsNoInstances = {
      expr = routed.systemd.services ? sprout-route && !(routed.systemd.services ? sprout-runner);
      expected = true;
    };
    testNothingWithoutInstancesOrRouter = {
      expr = (host { }).systemd.services ? sprout-route;
      expected = false;
    };
  };
in
if failures == [ ] then
  pkgs.runCommand "nixos-module" { } "touch $out"
else
  throw "nixos module: ${builtins.toJSON failures}"
