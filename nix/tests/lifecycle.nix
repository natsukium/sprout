# One instance through boot, exec, logs, stop/start, an abrupt daemon death,
# and deletion, checking after each way of going down that nothing it ran
# is left: no VM or sidecar process, no handle on var.img, no socket file.
{ ... }:
{
  vm = { };

  testScript = ''
    up a
    dir=$(instance_dir a)
    test "$(guest a 'echo hello')" = hello
    test "$(inspect a .state)" = running

    guest a 'echo sprout-console-marker >/dev/console'
    console_has_marker() {
      sprout logs --instance a -n 500 | grep -q sprout-console-marker
    }
    eventually 10 console_has_marker || fail "console output missing from sprout logs"

    pids=$(vm_processes a)
    sprout stop --instance a
    test "$(inspect a .state)" = stopped
    # shellcheck disable=SC2086
    assert_quiescent "$dir" $pids

    sprout start --instance a
    guest a true

    pids=$(vm_processes a)
    kill -KILL "$(inspect a .pid)"
    # On Linux the daemon's death takes the VM and its sidecars down with it;
    # elsewhere they may linger until the next boot reaps them.
    if [ "$(uname -s)" = Linux ]; then
      # shellcheck disable=SC2086
      eventually 30 none_alive $pids || fail "VM processes outlived a killed daemon"
      if lsof -t -- "$dir/var.img" >/dev/null 2>&1; then
        fail "var.img is still open after the daemon was killed"
      fi
    fi
    sprout start --instance a
    guest a true
    # shellcheck disable=SC2086
    alive $pids && fail "a boot after a killed daemon left its processes running"

    pids=$(vm_processes a)
    sprout stop --instance a
    # shellcheck disable=SC2086
    assert_quiescent "$dir" $pids

    sprout start --instance a
    pids=$(vm_processes a)
    sprout delete --force --instance a
    # shellcheck disable=SC2086
    assert_quiescent "$dir" $pids
    test ! -e "$dir" || fail "instance directory survived delete"
    for link in "/tmp/sprout-$(id -u)"/*; do
      if [ "$(readlink "$link")" = "$dir" ]; then
        fail "socket directory link $link survived delete"
      fi
    done
    if sprout inspect --instance a >/dev/null 2>&1; then
      fail "deleted instance still resolves"
    fi
  '';
}
