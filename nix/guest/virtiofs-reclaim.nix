# Virtualization.framework keeps a host file open for each cached virtiofs
# inode until the guest evicts it. A guest without memory pressure can keep
# those inodes indefinitely, so the host's sprout daemon calls
# sprout-virtiofs-reclaim.socket when the host file table runs short.
{ guest }:
{ config, pkgs, ... }:
{
  # systemd needs cgroup BPF to enforce the gateway allowlist.
  system.requiredKernelConfig = [ (config.lib.kernelConfig.isYes "CGROUP_BPF") ];

  networking.firewall.allowedTCPPorts = [ guest.reclaimPort ];

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

        before=$(fuse_inodes)
        after=$before
        if (( $(uptime_centiseconds) - last_drop >= cooldown )); then
          echo 2 > /proc/sys/vm/drop_caches
          uptime_centiseconds > "$stamp"
          # FUSE frees the inode slab objects in deferred RCU callbacks.
          sleep 1
          after=$(fuse_inodes)
        fi
        printf '%s %s\n' "$before" "$after"
      '';
    };
  };
}
