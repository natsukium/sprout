# `sprout up` returns only once the cluster can be acted on, so a script's
# first kubectl does not race a k3s that has merely started.
{ ... }:
{
  vm = {
    mem = 2048;
    modules = [ ../modules/nixos/k3s.nix ];
  };

  testScript = ''
    up k3s-ready

    # --timeout=0 checks once and does not wait: it passes only if `up` already
    # waited, which is the contract under test.
    guest k3s-ready 'kubectl wait --for=condition=Ready node --all --timeout=0'

    # The kubeconfig has to survive leaving the guest, which is how it is used
    # from the host through `sprout forward 6443`.
    guest k3s-ready 'grep -q certificate-authority-data /etc/rancher/k3s/k3s.yaml'
  '';
}
