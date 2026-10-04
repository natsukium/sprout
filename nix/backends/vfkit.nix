{ guest, socketPlaceholder }:
let
  network = "net.sock";
  control = "vfkit-rest.sock";
in
{
  microvm = {
    hypervisor = "vfkit";
    # Absolute, because microvm.nix prefixes a relative socket with the
    # runner's cwd — the instance state dir, whose depth is unbounded (see
    # socketdir.go).
    socket = socketPlaceholder control;
    # vfkit logs the allocated PTY path at info level; `sprout` parses it
    # from the runner output to attach the console.
    vfkit.logLevel = "info";
    vfkit.extraArgs = [
      "--device"
      "virtio-net,unixSocketPath=${socketPlaceholder network},mac=${guest.mac}"
    ];
  };

  backend = {
    kind = "vfkit";
    network = {
      transport = "vfkit-unixgram";
      socket = network;
    };
    control = {
      protocol = "vfkit-rest";
      socket = control;
    };
    console.mode = "pty-announce";
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
    {
      # vfkit's stdio console is unavailable on macOS 26, so attach
      # through its supported PTY console instead.
      placeholder = "virtio-serial,stdio";
      value = "consolePty";
    }
  ];
}
