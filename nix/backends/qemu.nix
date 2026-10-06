{
  guest,
  mem,
  hostPkgs,
  placeholderFor,
  shares,
  shareOwners,
}:
let
  inherit (hostPkgs) lib;
  socketPlaceholder = placeholderFor "sock";
  network = "net.sock";
  control = "vm-control.sock";
  netdev = "sprout0";

  # QEMU's microvm machine reserves no room for its ACPI tables when placing
  # the initrd at the top of low RAM, and the linuxboot option ROM moves it
  # below them except when that top is exactly the kernel's 2 GiB
  # initrd_addr_max: then, for any initrd size, its last page shares the
  # tables' page, and the guest hangs in ACPI init whenever the initrd's tail
  # reaches them. Low RAM equals the requested size up to 3 GiB, so 2048 MiB
  # is the only size with that layout.
  hangingMicrovmMem = 2048;

  # By index: tags embed freeform credential and cache names, which can break
  # the socket-name rule or overflow sun_path.
  shareSocket = i: "fs-${toString i}.sock";
  placedShares = lib.imap0 (i: s: s // { socket = socketPlaceholder (shareSocket i); }) shares;

  # What a sidecar is built from, with microvm.nix's defaults filled in;
  # older microvm.nix releases have no extraArgs option at all.
  sidecarFieldsOf = s: {
    inherit (s)
      proto
      tag
      socket
      source
      readOnly
      ;
    cache = s.cache or "auto";
    extraArgs = s.extraArgs or [ ];
  };

  hostId = {
    uid = placeholderFor "host" "uid";
    gid = placeholderFor "host" "gid";
  };

  # Rootless virtiofsd gets no uid 0 in its namespace, so guest ids are
  # squashed onto the host user's: untranslated, any chown by guest root
  # into a share fails with EINVAL. The reverse mapping shows the host
  # user's files to guest root as its own; otherwise git in the guest
  # refuses the workspace as owned by someone else. A cache with an owner
  # maps to that owner instead, since a non-root tool could neither enter a
  # root-owned 0700 cache nor write into the subdirectories it creates there.
  sidecars = lib.imap0 (
    i: share:
    let
      s = sidecarFieldsOf share;
      owner =
        shareOwners.${s.tag} or {
          uid = 0;
          gid = 0;
        };
    in
    {
      name = "virtiofsd-${s.tag}";
      exec = [
        "${hostPkgs.virtiofsd}/bin/virtiofsd"
        "--socket-path=${s.socket}"
        "--shared-dir=${s.source}"
        "--sandbox=namespace"
        "--cache=${s.cache}"
        "--translate-uid=squash-guest:0:${hostId.uid}:4294967295"
        "--translate-gid=squash-guest:0:${hostId.gid}:4294967295"
        "--translate-uid=host:${hostId.uid}:${toString owner.uid}:1"
        "--translate-gid=host:${hostId.gid}:${toString owner.gid}:1"
      ]
      ++ lib.optional s.readOnly "--readonly"
      ++ s.extraArgs;
      ready.socket = shareSocket i;
    }
  ) placedShares;

in
{
  shares = placedShares;

  module =
    { config, lib, ... }:
    let
      microvmMachine = config.microvm.qemu.machine == "microvm";
      varVolume = lib.findFirst (v: v.image == "var.img") null config.microvm.volumes;
    in
    {
      microvm = {
        hypervisor = "qemu";
        # Ahead of microvm.nix's own creation, which then finds the image and
        # skips it: that one marks the file No_COW, and btrfs refuses to
        # reflink a No_COW file, so every snapshot and fork would be a full
        # copy and `--live` would be refused.
        preStart = lib.optionalString (varVolume != null && varVolume.autoCreate) ''
          if [ ! -e var.img ]; then
            ${hostPkgs.coreutils}/bin/truncate -s ${toString varVolume.size}M var.img
            ${hostPkgs.e2fsprogs}/bin/mkfs.ext4 ${
              lib.escapeShellArgs (
                lib.optionals (varVolume.label != null) [
                  "-L"
                  varVolume.label
                ]
                ++ lib.optionals (varVolume.mkfsExtraArgs != null) varVolume.mkfsExtraArgs
              )
            } var.img
          fi
        '';
        # Absolute for the same reason as vfkit's: microvm.nix resolves a
        # relative socket against the deep instance directory.
        socket = socketPlaceholder control;
        # Over bundle.nix's plain definition, under a user's mkForce, which
        # the assertion below then refuses.
        mem = lib.mkIf (microvmMachine && mem == hangingMicrovmMem) (
          lib.mkOverride 90 (hangingMicrovmMem + 2)
        );
        # `microvm.interfaces` cannot describe a stream netdev, so the NIC is
        # added here; sprout always has virtiofs shares, which keep QEMU's
        # devices on PCI on every machine type.
        qemu.extraArgs = [
          "-netdev"
          "stream,id=${netdev},server=off,addr.type=unix,addr.path=${socketPlaceholder network}"
          "-device"
          "virtio-net-pci,netdev=${netdev},mac=${guest.mac},romfile="
        ];
      };

      assertions = [
        {
          # The runner's QEMU is pinned in the bundle, so an old one is
          # refused here rather than as an opaque `-netdev` parse error at boot.
          assertion = lib.versionAtLeast config.microvm.qemu.package.version "7.2";
          message = "sprout: the qemu backend needs QEMU 7.2 or newer for its stream netdev; microvm.qemu.package is ${config.microvm.qemu.package.version}";
        }
        {
          assertion = !(microvmMachine && config.microvm.mem == hangingMicrovmMem);
          message = "sprout: QEMU's microvm machine hangs at boot with exactly ${toString hangingMicrovmMem} MiB of memory; set sprout.vms.<name>.mem and leave microvm.mem alone";
        }
        {
          # The checks above read the options the runner renders as -M and -m;
          # a later copy in extraArgs, in either of QEMU's dash forms, would
          # win at runtime unchecked.
          assertion =
            !lib.any (lib.flip lib.elem (
              lib.concatMap
                (opt: [
                  "-${opt}"
                  "--${opt}"
                ])
                [
                  "M"
                  "machine"
                  "m"
                ]
            )) config.microvm.qemu.extraArgs;
          message = "sprout: set memory through sprout.vms.<name>.mem and the machine through microvm.qemu.machine, not microvm.qemu.extraArgs";
        }
        {
          # Each share needs the sidecar generated from it; one added or
          # altered through `modules` would leave QEMU dialing a socket nobody
          # serves, or serve it with the original source and permissions.
          assertion =
            map sidecarFieldsOf (lib.filter (s: s.proto == "virtiofs") config.microvm.shares)
            == map sidecarFieldsOf placedShares;
          message = "sprout: the qemu backend serves only the shares sprout declares; set them through sprout.vms.<name> (workspace, credentials, caches), not microvm.shares";
        }
      ];
    };

  backend = {
    kind = "qemu";
    network = {
      transport = "qemu-stream";
      socket = network;
    };
    control = {
      protocol = "qmp";
      socket = control;
    };
    console.mode = "stdio";
    inherit sidecars;
  };

  substitutions = [
    {
      placeholder = socketPlaceholder network;
      value = "socket:${network}";
    }
    {
      placeholder = socketPlaceholder control;
      value = "socket:${control}";
    }
    {
      placeholder = hostId.uid;
      value = "hostUid";
    }
    {
      placeholder = hostId.gid;
      value = "hostGid";
    }
  ]
  ++ lib.imap0 (i: _: {
    placeholder = socketPlaceholder (shareSocket i);
    value = "socket:${shareSocket i}";
  }) placedShares;
}
