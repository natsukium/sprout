# A guest HTTP server reached through `forward` and through the router, the
# idle stop with nothing attached, and the router waking the stopped
# instance on the next request. One test because the wake needs the idle
# stop and the idle stop needs a router that does not hold the VM open.
{ ... }:
{
  vm = {
    idle.after = "20s";
    modules = [
      (
        { pkgs, ... }:
        {
          services.nginx = {
            enable = true;
            virtualHosts.default = {
              default = true;
              root = pkgs.writeTextDir "index.html" "sprout-http-ok";
            };
          };
        }
      )
    ];
  };

  testScript = ''
    forward_port=$((20000 + RANDOM % 20000))
    route_port=$((forward_port + 1))
    up a
    dir=$(instance_dir a)

    bg sprout forward --instance a "$forward_port:80"
    forwarded() {
      curl -fsS "http://127.0.0.1:$forward_port/" 2>/dev/null | grep -q sprout-http-ok
    }
    eventually 10 forwarded || fail "forwarded port does not reach the guest"

    bg sprout route serve --port "$route_port"
    routed() {
      curl -fsS -H "Host: a.sprout.localhost" "http://127.0.0.1:$route_port/" 2>/dev/null | grep -q sprout-http-ok
    }
    eventually 10 routed || fail "router does not reach the guest"

    pids=$(vm_processes a)
    stopped() {
      [ "$(inspect a .state)" = stopped ]
    }
    eventually 90 stopped || fail "idle instance did not stop"
    # shellcheck disable=SC2086
    assert_quiescent "$dir" $pids

    status=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: a.sprout.localhost" "http://127.0.0.1:$route_port/")
    test "$status" = 503 || fail "router answered $status for a stopped instance, not its waking page"
    eventually 120 routed || fail "router did not wake the instance"
  '';
}
