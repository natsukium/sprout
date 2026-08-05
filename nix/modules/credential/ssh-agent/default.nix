# The host ssh-agent socket is proxied over the virtual network; keys never
# enter the VM. The gateway listener is reachable by anything with network
# access inside the guest, containers included: the same reach `ssh -A` grants.
{ lib, ... }:
{
  options.credentials.ssh-agent = lib.mkOption {
    type = lib.types.submodule [
      ../entry.nix
      (
        { config, ... }:
        {
          config = {
            strategy = lib.mkDefault "socket";
            source = lib.mkDefault "$SSH_AUTH_SOCK";
            guestPort = lib.mkDefault 62222;
            guestPath = lib.mkDefault "/run/ssh-agent.sock";
            guestEnv.SSH_AUTH_SOCK = lib.mkDefault config.guestPath;
          };
        }
      )
    ];
    default = { };
    description = "The host SSH_AUTH_SOCK, forwarded over the virtual network.";
  };
}
