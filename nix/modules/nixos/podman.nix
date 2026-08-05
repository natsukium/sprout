# Runtime-created network definitions must follow images and volumes onto
# persistent /var; leaving them under Podman's rootful /etc default strands
# otherwise-persistent containers after reboot.
{
  config,
  lib,
  ...
}:
let
  networkConfigDir = "/var/lib/containers/networks";
in
{
  virtualisation.podman = {
    enable = true;
    # An explicit no-op setting makes the NixOS module materialize podman.json,
    # so changing network_config_dir does not bypass defaultNetwork.settings.
    defaultNetwork.settings.dns_enabled = lib.mkDefault false;
  };

  # This profile follows sprout's root login workflow. Applying the override in
  # base.nix would also redirect rootless Podman away from its per-user graphroot.
  virtualisation.containers.containersConf.settings.network.network_config_dir =
    lib.mkDefault networkConfigDir;

  systemd.tmpfiles.settings."10-sprout-podman-networks" = {
    ${networkConfigDir}.d.mode = "0755";
    "${networkConfigDir}/podman.json"."L+".argument =
      toString
        config.environment.etc."containers/networks/podman.json".source;
  };
}
