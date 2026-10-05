# A socket credential proxies a host agent into the guest without the key
# ever crossing: the guest lists the host agent's key through the forwarded
# socket.
{ ... }:
{
  vm.credentials.ssh-agent.enable = true;

  testScript = ''
    eval "$(ssh-agent -a "$SPROUT_TEST_DIR/agent.sock")" >/dev/null
    background+=("$SSH_AGENT_PID")
    ssh-keygen -q -t ed25519 -N "" -C sprout-test-key -f "$SPROUT_TEST_DIR/key"
    ssh-add -q "$SPROUT_TEST_DIR/key"
    up a

    guest a 'SSH_AUTH_SOCK=/run/ssh-agent.sock ssh-add -l' | grep -q sprout-test-key || fail "guest cannot list the host agent's key"
  '';
}
