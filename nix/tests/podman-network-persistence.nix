# A rootful Podman network keeps its identity across a full guest reboot.
{ ... }:
{
  vm.modules = [
    ../modules/nixos/podman.nix
    { virtualisation.podman.defaultNetwork.settings.dns_enabled = true; }
  ];

  testScript = ''
    up podman-network

    guest podman-network 'grep -q "dns_enabled.*true" /var/lib/containers/networks/podman.json'
    guest podman-network 'podman network create reboot-test >/dev/null'
    before=$(guest podman-network "podman network inspect --format '{{.ID}}' reboot-test")

    sprout stop --instance podman-network
    sprout start --instance podman-network

    after=$(guest podman-network "podman network inspect --format '{{.ID}}' reboot-test")
    test "$before" = "$after"
  '';
}
