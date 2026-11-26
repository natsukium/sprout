# Virtualization.framework holds a host file open per cached virtiofs inode, so
# the macOS daemon dials this socket to evict them under host table pressure.
{ guest }:
{
  config,
  lib,
  pkgs,
  ...
}:
let
  enabled = config.systemd.sockets.sprout-virtiofs-reclaim.enable;
in
{
  # IPAddressAllow requires CGROUP_BPF.
  system.requiredKernelConfig = lib.optional enabled (config.lib.kernelConfig.isYes "CGROUP_BPF");

  networking.firewall.allowedTCPPorts = lib.optional enabled guest.reclaimPort;

  systemd.sockets.sprout-virtiofs-reclaim = {
    wantedBy = [ "sockets.target" ];
    socketConfig = {
      ListenStream = toString guest.reclaimPort;
      Accept = true;
      MaxConnections = 1;
      IPAddressAllow = guest.gatewayIp;
      IPAddressDeny = "any";
    };
  };

  systemd.services."sprout-virtiofs-reclaim@" = {
    serviceConfig = {
      StandardOutput = "socket";
      StandardError = "journal";
      ExecStart = pkgs.writeShellScript "sprout-virtiofs-reclaim" ''
        set -eu
        fuse_inodes() {
          ${pkgs.gawk}/bin/awk '$1 == "fuse_inode" { count = $2; exit } END { print count + 0 }' /proc/slabinfo
        }
        uptime_centiseconds() {
          local uptime
          read -r uptime _ < /proc/uptime
          printf '%d\n' "$((10#''${uptime/./}))"
        }
        # A forwarded connection can trigger reclaim. Use uptime so guest
        # clock changes cannot bypass the limit.
        cooldown=$((30 * 100))
        stamp=/run/sprout-virtiofs-reclaim.last-drop
        last_drop=$((-cooldown))
        if [[ -f "$stamp" ]]; then
          read -r last_drop < "$stamp"
        fi

        if (( $(uptime_centiseconds) - last_drop < cooldown )); then
          printf 'skipped\n'
          exit 0
        fi

        before=$(fuse_inodes)
        echo 2 > /proc/sys/vm/drop_caches
        uptime_centiseconds > "$stamp"
        # FUSE frees the inode slab objects in deferred RCU callbacks.
        sleep 1
        after=$(fuse_inodes)
        printf '%s %s\n' "$before" "$after"
      '';
    };
  };
}
