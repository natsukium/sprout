# Not bootable yet: the network device and the virtiofs sidecars are still
# missing. The manifest declares the whole contract regardless, so a Linux
# bundle validates against the binary that will boot it.
{ socketPlaceholder, ... }:
let
  control = "vm-control.sock";
in
{
  microvm = {
    hypervisor = "qemu";
    # Absolute for the same reason as vfkit's: microvm.nix resolves a
    # relative socket against the deep instance directory.
    socket = socketPlaceholder control;
  };

  backend = {
    kind = "qemu";
    network = {
      transport = "qemu-stream";
      socket = "net.sock";
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
      placeholder = socketPlaceholder control;
      value = "socket:${control}";
    }
  ];
}
