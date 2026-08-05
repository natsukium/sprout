{ guest, dataMount }:
{ lib, pkgs, ... }:
let
  guestGlobalEnv = {
    # A stable marker for "am I inside a sprout guest?" so project scripts can
    # gate behavior without inventing their own flag.
    SPROUT_GUEST = "1";
    # The corrected xtables directory the unit below rebuilds; see its comment
    # for why the store's own copy is unusable here.
    XTABLES_LIBDIR = "/var/lib/xtables-fixed";
  };
in
{
  # authorized_keys arrives via the sprout-data virtiofs share at boot time, so
  # images stay key-free and instances stay disposable. An
  # AuthorizedKeysCommand sidesteps sshd's ownership checks, which would
  # reject the file because virtiofs preserves the host-side (non-root) uid.
  services.openssh = {
    enable = true;
    settings.PermitRootLogin = "prohibit-password";
    authorizedKeysCommand = "/etc/ssh/sprout-authorized-keys";
    authorizedKeysCommandUser = "root";
    # /etc is rebuilt from the Nix store every boot; only /var survives
    # `sprout stop` + `sprout up`. Without this, sshd mints a new host key on every
    # boot and every reconnect trips known_hosts key-change warnings.
    hostKeys = [
      {
        path = "/var/lib/ssh/ssh_host_ed25519_key";
        type = "ed25519";
      }
    ];
  };
  environment.etc."ssh/sprout-authorized-keys" = {
    mode = "0555";
    text = ''
      #!/bin/sh
      # sshd runs this with a stripped PATH pointing at /usr/bin:/bin, which
      # are (near) empty on NixOS — every binary must be a store path.
      exec ${pkgs.coreutils}/bin/cat ${dataMount}/ssh/authorized_keys 2>/dev/null || true
    '';
  };

  # Serial console autologin: when SSH is broken, the PTY console captured
  # by `sprout logs` must be enough to debug the guest.
  services.getty.autologinUser = "root";

  networking.useDHCP = lib.mkDefault true;

  # Containers and pods started in the guest inherit its /etc/resolv.conf into
  # their own network namespace, so that file has to name a resolver reachable
  # from there. systemd-resolved publishes its stub, 127.0.0.53, which in a
  # copied file addresses the copier instead: k3s's CoreDNS would query itself,
  # and docker rejects a loopback-only file and substitutes a public resolver.
  # Its fallback servers are a second reason to skip it, since they would route
  # dev-domain queries to public DNS whenever the link scope failed. Not
  # mkDefault: networkd turns resolved on at that same priority, so a guest
  # that wants the stub back needs lib.mkForce.
  services.resolved.enable = false;

  # Naming the gateway statically rather than leaving it to DHCP: networkd
  # holds a lease's nameservers for resolved alone and writes /etc/resolv.conf
  # through no other path, so with resolved off the file would list none. The
  # address is fixed by the same bundle that sets the static lease.
  networking.nameservers = [ guest.gatewayIp ];

  # The embedded gvisor-tap-vsock stack is IPv4-only: tools that try ::1
  # first report "Connection refused" against services that are actually up.
  # Guests that need IPv6 internally can override, but nothing routes it.
  networking.enableIPv6 = lib.mkDefault false;

  # The isolation boundary is the VM itself (the same reasoning that keeps
  # hostLoopback off by default): the guest is reachable only through the
  # embedded network stack the host daemon dials, so an in-guest firewall
  # only breaks `sprout forward` and in-VM container networking.
  networking.firewall.enable = lib.mkDefault false;

  nix.settings.experimental-features = lib.mkDefault [
    "nix-command"
    "flakes"
  ];

  environment.variables = guestGlobalEnv;
  systemd.globalEnvironment = guestGlobalEnv;

  # Instance identity (SPROUT_INSTANCE_NAME / SPROUT_INSTANCE_ID / SPROUT_DEFINITION)
  # arrives per-boot through the data share, never the image, so one bundle
  # serves every branch. systemd units read the same file with
  # `EnvironmentFile = "-/run/sprout/instance.env"`.
  environment.loginShellInit = ''
    if [ -r ${dataMount}/instance.env ]; then
      set -a
      . ${dataMount}/instance.env
      set +a
    fi
  '';

  # The host /nix/store lives on case-insensitive APFS, so its virtiofs share
  # carries Nix's case-hack names: libxt_mark.so (colliding with
  # libxt_MARK.so) is stored as libxt_mark.so~nix~case~hack~1, iptables cannot
  # find it, and every `-m mark` rule — hence all container networking —
  # fails. Rebuild a corrected xtables directory and point XTABLES_LIBDIR at
  # it for login sessions and every systemd unit alike.
  systemd.services.fix-iptables-case-hack = {
    description = "Restore case-hacked iptables extension filenames";
    wantedBy = [ "multi-user.target" ];
    before = [
      "docker.service"
      "k3s.service"
    ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
      ExecStart = pkgs.writeShellScript "fix-iptables-case-hack" ''
        set -euo pipefail
        dst=/var/lib/xtables-fixed
        rm -rf "$dst"
        mkdir -p "$dst"
        for f in ${pkgs.iptables.lib}/lib/xtables/*; do
          [ -f "$f" ] || continue
          base=$(basename "$f")
          cp -f "$f" "$dst/''${base%%\~nix\~case\~hack\~*}"
        done
      '';
    };
  };

  # Readiness beyond "sshd answers": a unit that must complete before the
  # host announces "VM ready" hooks in with
  #   requiredBy = [ "sprout-ready.target" ]; before = [ "sprout-ready.target" ];
  # and the notifier then touches the marker `sprout up` waits for. requires=
  # (not wants=) so a failed readiness unit keeps the marker unwritten and
  # the host reports a timeout instead of a half-ready instance.
  systemd.targets.sprout-ready = {
    description = "Guest-declared readiness goal";
  };
  systemd.services.sprout-ready-notify = {
    description = "Signal readiness to the host through the data share";
    requires = [ "sprout-ready.target" ];
    after = [ "sprout-ready.target" ];
    wantedBy = [ "multi-user.target" ];
    unitConfig.RequiresMountsFor = [ dataMount ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
      ExecStart = "${pkgs.coreutils}/bin/touch ${dataMount}/ready";
    };
  };

  documentation.enable = lib.mkDefault false;

  system.stateVersion = lib.mkDefault "25.11";
}
