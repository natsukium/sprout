# /var is the only volume that survives a stop, so an instance cache placed
# anywhere else would be wiped by the next boot.
{ instanceCaches }:
{ pkgs, lib, ... }:
{
  # A plain `fileSystems` bind entry cannot be used: its source has to exist
  # before local-fs.target, which is exactly when systemd-tmpfiles would
  # create it, so the two order against each other. One unit doing both in
  # sequence has no such cycle.
  systemd.services.sprout-instance-caches = {
    description = "Bind instance-scoped caches from the persistent volume";
    wantedBy = [ "multi-user.target" ];
    # `up` reports the VM ready as soon as sshd answers, so a cache mounted
    # after it could be raced by the first guest command — and a tool that
    # wrote to the unmounted guestPath first would leave files stranded on
    # the root tmpfs.
    before = [
      "sshd.service"
      "sprout-ready.target"
    ];
    requiredBy = [ "sprout-ready.target" ];
    # Each guestPath, not just its parent: if a consumer mounts something at
    # the same path (e.g. a tmpfs masking a workspace dir), the bind must be
    # ordered after it so the cache lands on top.
    unitConfig.RequiresMountsFor = [ "/var" ] ++ lib.mapAttrsToList (_: c: c.guestPath) instanceCaches;
    path = with pkgs; [
      coreutils
      util-linux
    ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
      ExecStart = lib.mapAttrsToList (
        cname: c:
        "${pkgs.writeShellScript "sprout-instance-cache-${cname}" ''
          set -euo pipefail
          mkdir -p /var/sprout-cache/${cname} ${c.guestPath}
          # Idempotent: the unit re-runs on a `systemctl restart` and after a
          # switch, where the bind is already in place.
          mountpoint -q ${c.guestPath} || mount --bind /var/sprout-cache/${cname} ${c.guestPath}
        ''}"
      ) instanceCaches;
    };
  };
}
