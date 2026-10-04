# Not bootable yet: QMP control, the stdio console and the virtiofs sidecars
# are still missing. The manifest declares the whole contract regardless, so
# a Linux bundle validates against the binary that will boot it.
{ guest, socketPlaceholder }:
let
  network = "net.sock";
  control = "vm-control.sock";
  netdev = "sprout0";
in
{
  module =
    { config, lib, ... }:
    {
      microvm = {
        hypervisor = "qemu";
        # Absolute for the same reason as vfkit's: microvm.nix resolves a
        # relative socket against the deep instance directory.
        socket = socketPlaceholder control;
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
