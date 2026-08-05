# A k3s node returns with the same cluster identity after a full guest reboot.
{ ... }:
{
  vm = {
    mem = 2048;
    modules = [ ../modules/nixos/k3s.nix ];
  };

  testScript = ''
    node_uid() {
      guest k3s-state 'for i in $(seq 1 180); do uid=$(kubectl get nodes -o jsonpath="{.items[0].metadata.uid}" 2>/dev/null) && test -n "$uid" && { printf %s "$uid"; exit 0; }; sleep 1; done; exit 1'
    }

    up k3s-state
    before=$(node_uid)

    sprout stop --instance k3s-state
    sprout start --instance k3s-state

    after=$(node_uid)
    test "$before" = "$after"
  '';
}
