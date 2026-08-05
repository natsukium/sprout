{
  lib,
  pkgs,
  config,
  ...
}:
let
  cfg = config.sprout.k3s;
in
{
  options.sprout.k3s = {
    kubeconfig = lib.mkOption {
      type = lib.types.str;
      default = "/etc/rancher/k3s/k3s.yaml";
      description = ''
        Where k3s writes its kubeconfig. This module's own units read it to
        reach the cluster, and it seeds KUBECONFIG for logins. A project that
        moves the file with `--write-kubeconfig` sets this to match; a project
        that only wants a different KUBECONFIG for its shells overrides that
        variable instead, leaving the units pointed at the real file.
      '';
    };

    readyTarget = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = "sprout-ready.target";
      description = ''
        Systemd target held back until the k3s node reports Ready, so that an
        instance counts as usable only once kubectl can act on it. Set to null
        to let readiness mean "k3s started" and leave the waiting to callers.
      '';
    };
  };

  config = {
    services.k3s = {
      enable = true;
      role = lib.mkDefault "server";
    };

    # k3s normally modprobes what it needs, but the netfilter match extensions
    # (xt_*) are resolved lazily per iptables rule, inside containers as well,
    # where /lib/modules is not the guest's. Preloading the set k3s's kube-proxy
    # and common CNI/ServiceLB rules use makes rule installation deterministic.
    boot.kernelModules = [
      "br_netfilter"
      "overlay"
      "ip_tables"
      "iptable_nat"
      "iptable_filter"
      "iptable_mangle"
      "nf_conntrack"
      "nf_nat"
      "veth"
      "xt_mark"
      "xt_conntrack"
      "xt_comment"
      "xt_multiport"
      "xt_addrtype"
      "xt_connmark"
      "xt_recent"
      "xt_set"
      "xt_statistic"
      "xt_MASQUERADE"
      "xt_REDIRECT"
    ];

    # Safe to set at boot despite the usual "let k3s set them" advice: the
    # bridge-nf keys only exist once br_netfilter is loaded, and the explicit
    # kernelModules entry above orders systemd-modules-load before
    # systemd-sysctl, so there is no load/set race here.
    boot.kernel.sysctl = {
      "net.bridge.bridge-nf-call-iptables" = 1;
      "net.bridge.bridge-nf-call-ip6tables" = 1;
      "net.ipv4.ip_forward" = 1;
    };

    environment.variables.KUBECONFIG = lib.mkDefault cfg.kubeconfig;
    environment.systemPackages = [ pkgs.kubectl ];

    # k3s already listens on every interface, so `sprout forward 6443` reaches
    # it without help; a guest that sets `--bind-address=0.0.0.0` anyway gets a
    # kubeconfig naming the literal 0.0.0.0, which no client can dial. Pinning
    # the server to 127.0.0.1 repairs that and is a no-op otherwise, and
    # embedding the CA keeps the file usable after it leaves the guest, which
    # is how it is used from the host. partOf re-runs this on k3s restart so a
    # regenerated k3s.yaml cannot resurrect either problem.
    systemd.services.sprout-k3s-kubeconfig = {
      description = "Make the k3s kubeconfig dialable and self-contained";
      after = [ "k3s.service" ];
      wants = [ "k3s.service" ];
      partOf = [ "k3s.service" ];
      wantedBy = [ "multi-user.target" ];
      path = [
        pkgs.kubectl
        pkgs.coreutils
      ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        Environment = "KUBECONFIG=${cfg.kubeconfig}";
      };
      script = ''
        until [ -s ${cfg.kubeconfig} ]; do sleep 1; done
        kubectl config set-cluster default \
          --server=https://127.0.0.1:6443 \
          --certificate-authority=/var/lib/rancher/k3s/server/tls/server-ca.crt \
          --embed-certs=true
      '';
    };

    # k3s.service is up as soon as the server process starts, minutes before a
    # single-node cluster can schedule anything. Without this, `sprout up`
    # returns on sshd and the first kubectl a caller runs hits an API server
    # that is not serving yet.
    systemd.services.sprout-k3s-ready = lib.mkIf (cfg.readyTarget != null) {
      description = "Wait for the k3s node to become schedulable";
      after = [
        "k3s.service"
        "sprout-k3s-kubeconfig.service"
      ];
      wants = [
        "k3s.service"
        "sprout-k3s-kubeconfig.service"
      ];
      requiredBy = [ cfg.readyTarget ];
      before = [ cfg.readyTarget ];
      path = [ pkgs.kubectl ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        Environment = "KUBECONFIG=${cfg.kubeconfig}";
        # A cold guest unpacks images and bootstraps etcd before the node
        # registers; the host allows a gated boot 10m, so failing earlier
        # would only replace a slow boot with a failed one.
        TimeoutStartSec = "10min";
      };
      # `kubectl wait` errors rather than blocks while the API server is
      # unreachable or no node object exists yet, so the retry is the wait.
      script = ''
        until kubectl wait --for=condition=Ready node --all --timeout=30s; do
          sleep 2
        done
      '';
    };
  };
}
