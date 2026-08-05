# A function over the enabled credentials (not a module with options) because
# share tags and mount points are fixed when the bundle is built.
{
  guest,
  dataMount,
  mountCreds,
  materializeCreds,
  socketCreds,
}:
{ lib, pkgs, ... }:
{
  # vfkit's virtio-fs device carries no read-only flag, so ro is enforced
  # guest-side; mkAfter puts it after microvm's own mount options, where it
  # wins.
  fileSystems = lib.mapAttrs' (_: c: lib.nameValuePair c.target { options = lib.mkAfter [ "ro" ]; }) (
    lib.filterAttrs (_: c: c.readOnly) mountCreds
  );
  # Materialized credentials are copied (not symlinked) out of the data
  # share each boot, so the file gets the root-owned 0600 perms
  # credential-sensitive tools may check.
  systemd.tmpfiles.rules = lib.concatLists (
    lib.mapAttrsToList (cname: c: [
      "d ${dirOf c.guestPath} 0700 root root - -"
      "C ${c.guestPath} 0600 root root - ${dataMount}/credentials/${cname}"
    ]) materializeCreds
  );
  # Socket strategy: a socket-activated proxy forwards the guest unix socket
  # to the daemon's in-stack listener on the gateway.
  systemd.sockets = lib.mapAttrs' (
    cname: c:
    lib.nameValuePair "sprout-socket-${cname}" {
      wantedBy = [ "sockets.target" ];
      socketConfig.ListenStream = c.guestPath;
    }
  ) socketCreds;
  systemd.services = lib.mapAttrs' (
    cname: c:
    lib.nameValuePair "sprout-socket-${cname}" {
      requires = [ "sprout-socket-${cname}.socket" ];
      after = [ "sprout-socket-${cname}.socket" ];
      serviceConfig.ExecStart = "${pkgs.systemd}/lib/systemd/systemd-socket-proxyd ${guest.gatewayIp}:${toString c.guestPort}";
    }
  ) socketCreds;
}
