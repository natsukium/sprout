# Not bootable yet: the virtiofs sidecars are still missing. The manifest declares the whole contract regardless, so
# a Linux bundle validates against the binary that will boot it.
{
  guest,
  mem,
  socketPlaceholder,
}:
let
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
in
{
  module =
    { config, lib, ... }:
    let
      microvmMachine = config.microvm.qemu.machine == "microvm";
    in
    {
      microvm = {
        hypervisor = "qemu";
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
    sidecars = [ ];
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
  ];
}
