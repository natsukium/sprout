# The runner bakes placeholder paths into its device arguments and the
# manifest tells `sprout` which to substitute per instance, so one pure
# microvm.nix runner, unmodified from upstream, serves any number of instances.
{ localInputs, lib }:
let
  systems = import ./systems.nix;
  backends = import ./backends;
  parseSize =
    v:
    if builtins.isInt v then
      v
    else
      let
        m = builtins.match "([0-9]+) *(([KMGT])i?B)?" v;
      in
      if m == null then
        throw "sprout: invalid size \"${v}\"; want an integer (MiB) or a string like \"8GiB\"/\"512MiB\""
      else
        let
          n = lib.toInt (builtins.elemAt m 0);
          unit = builtins.elemAt m 2;
          mib = {
            K = n / 1024;
            M = n;
            G = n * 1024;
            T = n * 1024 * 1024;
          };
        in
        if unit == null then n else mib.${unit};

  # Not a real path on either side: the runner carries it literally until
  # `rewriteRunner` swaps in the instance's own directory.
  placeholderRoot = "/sprout/placeholder";
  placeholderFor = kind: name: "${placeholderRoot}/${kind}/${name}";

  placeholders = {
    data = "${placeholderRoot}/data";
    workspace = "${placeholderRoot}/workspace";
    gitCommon = "${placeholderRoot}/gitcommon";
  };

  # Share this for every checkout because only `up` knows whether an instance
  # uses a worktree, after guest mount points are fixed. Primary checkouts get a
  # harmless second view of .git.
  gitCommonMount = "/run/sprout-git";

  dataMount = "/run/sprout";

  guest = {
    # A DHCP static lease in the embedded gvproxy stack pins the guest to
    # this IP, so the host side never has to discover it.
    ip = "192.168.127.2";
    gatewayIp = "192.168.127.1";
    subnet = "192.168.127.0/24";
    mac = "02:00:00:00:00:02";
    sshUser = "root";
  };
  vmParts =
    hostPkgs: name: vmCfg:
    let
      hostSystem = hostPkgs.stdenv.hostPlatform.system;
      # Derived from the host rather than exposed as an option, which would
      # accept combinations the runners cannot boot.
      guestSystem = systems.guestFor hostSystem;
      backend = backends.${systems.resolveBackend hostSystem vmCfg.backend} {
        inherit
          guest
          mem
          hostPkgs
          placeholderFor
          ;
        shares = baseShares;
      };
      mem = parseSize vmCfg.mem;
      # Which fields a credential needs depends on its strategy, so entry.nix
      # cannot mark any of them mandatory; checked here instead.
      credentials = lib.mapAttrs (
        cname: c:
        if c.strategy == null then
          throw "sprout: credential '${cname}' has no strategy; set credentials.${cname}.strategy (mount/materialize/socket)"
        else if c.strategy == "materialize" && c.exec == null then
          throw "sprout: materialize credential '${cname}' needs an exec command"
        else
          c
      ) (lib.filterAttrs (_: c: c.enable) vmCfg.credentials);
      credsFor = strategy: lib.filterAttrs (_: c: c.strategy == strategy) credentials;

      caches = lib.filterAttrs (_: c: c.enable) vmCfg.caches;
      # Only the scopes that outlive an instance need a host tree; an
      # instance-scoped cache is backed by the guest's own /var volume, which
      # already dies with `sprout delete` and spares it a virtiofs round trip.
      hostCaches = lib.filterAttrs (_: c: c.scope != "instance") caches;
      instanceCaches = lib.filterAttrs (_: c: c.scope == "instance") caches;

      mkPlaceholderMounts = kind: tagPrefix: guestPath: readOnly: attrs: {
        shares = lib.mapAttrsToList (n: c: {
          proto = "virtiofs";
          tag = "${tagPrefix}-${n}";
          source = placeholderFor kind n;
          mountPoint = guestPath c;
          readOnly = readOnly c;
        }) attrs;
        substitutions = lib.mapAttrsToList (n: _: {
          placeholder = placeholderFor kind n;
          value = "${kind}:${n}";
        }) attrs;
      };

      credMounts = mkPlaceholderMounts "credential" "cred" (c: c.target) (c: c.readOnly) (
        credsFor "mount"
      );
      cacheMounts = mkPlaceholderMounts "cache" "cache" (c: c.guestPath) (_: false) hostCaches;

      baseShares = [
        {
          proto = "virtiofs";
          tag = "ro-store";
          source = "/nix/store";
          mountPoint = "/nix/.ro-store";
          readOnly = true;
        }
        {
          # Per-instance data (SSH authorized_keys, materialized
          # credentials), populated by `sprout` at boot.
          proto = "virtiofs";
          tag = "sprout-data";
          source = placeholders.data;
          mountPoint = dataMount;
          readOnly = false;
        }
      ]
      ++ lib.optionals vmCfg.workspace [
        {
          proto = "virtiofs";
          tag = "workspace";
          source = placeholders.workspace;
          mountPoint = "/workspace";
          readOnly = false;
        }
        {
          proto = "virtiofs";
          tag = "sprout-git";
          source = placeholders.gitCommon;
          mountPoint = gitCommonMount;
          readOnly = false;
        }
      ]
      ++ credMounts.shares
      ++ cacheMounts.shares;
      cacheManifest = lib.mapAttrsToList (cname: c: {
        name = cname;
        inherit (c) scope;
      }) hostCaches;

      entries = lib.attrValues credentials ++ lib.attrValues caches;
      entryModules = lib.filter (m: m != null) (lib.catAttrs "guestModule" entries) ++ [
        { environment.variables = lib.mergeAttrsList (lib.catAttrs "guestEnv" entries); }
      ];

      credManifest = lib.mapAttrsToList (
        cname: c:
        {
          name = cname;
          inherit (c) strategy;
        }
        // {
          mount = { inherit (c) source; };
          materialize = { inherit (c) exec; };
          socket = { inherit (c) source guestPort; };
        }
        .${c.strategy}
      ) credentials;

      nixos = localInputs.nixpkgs.lib.nixosSystem {
        system = guestSystem;
        modules = [
          localInputs.microvm.nixosModules.microvm
          (import ./guest/base.nix {
            inherit guest dataMount;
            inherit (hostPkgs.stdenv) hostPlatform;
          })
          {
            networking.hostName = lib.mkDefault "sprout-${name}";
            microvm = {
              inherit (vmCfg) vcpu;
              inherit mem;
              vmHostPackages = hostPkgs;
              # The store comes in via virtiofs instead of an erofs image:
              # no image rebuild on every guest change.
              storeOnDisk = false;
              # The overlay's upper dir lives on the persistent /var volume,
              # not tmpfs: in-guest builds can exceed guest RAM, and store
              # paths built once should survive a stop/up cycle.
              writableStoreOverlay = lib.mkIf vmCfg.writableStore "/var/nix-rw-store";
              inherit (backend) shares;

              volumes = [
                {
                  image = "var.img";
                  mountPoint = "/var";
                  size = parseSize vmCfg.diskSize;
                }
              ];

              # Networking goes through the gvisor-tap-vsock stack embedded
              # in the `sprout` daemon, not vfkit's builtin NAT: NAT offers no
              # host->guest path, while the embedded stack gives
              # deterministic leases, in-process dialing (`sprout shell`
              # without host ports) and dynamic port forwarding.
              interfaces = [ ];
            };
          }
          (
            { config, ... }:
            {
              assertions = [
                {
                  # The daemon reads the ext4 superblock to tell a finished
                  # format from an interrupted one (cmd/sprout/orphan.go); any
                  # other filesystem would be set aside as unformatted on
                  # every boot.
                  assertion = lib.all (v: v.image != "var.img" || v.fsType == "ext4") config.microvm.volumes;
                  message = "sprout: the /var volume must stay ext4";
                }
              ];
            }
          )
          backend.module
          (import ./guest/credentials.nix {
            inherit guest dataMount;
            mountCreds = credsFor "mount";
            materializeCreds = credsFor "materialize";
            socketCreds = credsFor "socket";
          })
        ]
        ++ lib.optional (instanceCaches != { }) (
          import ./guest/instance-caches.nix { inherit instanceCaches; }
        )
        ++ entryModules
        ++ lib.optionals vmCfg.workspace [
          (import ./guest/worktree.nix { inherit gitCommonMount; })
          ./guest/workspace-shell.nix
        ]
        ++ vmCfg.modules;
      };

      manifest = {
        version = 2;
        definition = name;
        host.system = hostSystem;
        # system keys the host-side cache trees.
        guest = guest // {
          system = guestSystem;
        };
        inherit (backend) backend;
        workspace = vmCfg.workspace;
        hostLoopback = vmCfg.hostLoopback;
        idle = { inherit (vmCfg.idle) action after; };
        # Answered by the resolver the gateway already runs, so the guest keeps
        # the resolver DHCP hands it. A guest-side resolver on loopback would
        # be wrong for every consumer that copies /etc/resolv.conf into another
        # network namespace: k3s's CoreDNS would resolve 127.0.0.1 to itself,
        # and docker silently swaps a loopback-only file for a public resolver.
        dns = { inherit (vmCfg.dns) wildcardDomains; };
        credentials = credManifest;
        caches = cacheManifest;
        substitutions = [
          {
            placeholder = placeholders.data;
            value = "dataDir";
          }
        ]
        ++ lib.optionals vmCfg.workspace [
          {
            placeholder = placeholders.workspace;
            value = "workspace";
          }
          {
            placeholder = placeholders.gitCommon;
            value = "gitCommon";
          }
        ]
        ++ credMounts.substitutions
        ++ cacheMounts.substitutions
        ++ backend.substitutions;
      };

    in
    {
      inherit nixos manifest;
    };
in
{
  mkGuest =
    hostPkgs: name: vmCfg:
    (vmParts hostPkgs name vmCfg).nixos;

  mkBundle =
    hostPkgs: name: vmCfg:
    let
      inherit (vmParts hostPkgs name vmCfg) nixos manifest;
      manifestFile = hostPkgs.writeText "sprout-manifest-${name}.json" (builtins.toJSON manifest);
    in
    hostPkgs.runCommand "sprout-vm-${name}" { passthru = { inherit manifest nixos; }; } ''
      mkdir -p $out
      ln -s ${nixos.config.microvm.declaredRunner}/bin/microvm-run $out/runner
      cp ${manifestFile} $out/manifest.json
    '';
}
