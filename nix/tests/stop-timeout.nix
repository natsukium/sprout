# A process ignoring SIGTERM must not turn `sprout stop` into a forced power
# cut: the guest has to finish its own poweroff inside the host's wait. The
# second unit is the harder case, a PID-namespace init (what a container's
# PID 1 is) under KillMode=process, as k3s runs its containers: it survives
# its unit's stop and is only killed by systemd-shutdown.
{ ... }:
{
  vm = { };

  testScript = ''
    up deaf
    guest deaf 'systemd-run -E PATH=/run/current-system/sw/bin --unit=term-deaf sh -c "trap \"\" TERM; sleep 3600"'
    guest deaf 'systemd-run -E PATH=/run/current-system/sw/bin --unit=pidns-deaf -p KillMode=process unshare --pid --fork sleep 3600'
    guest deaf 'systemctl is-active --quiet term-deaf pidns-deaf'

    sprout stop --instance deaf
    state="$XDG_STATE_HOME/sprout/instances"
    if grep -rq "forcing stop" "$state"; then
      echo "FAIL: the host forced the VM off instead of the guest powering off"
      exit 1
    fi
    # Printed by the guest's systemd once every unit is stopped and /var is
    # unmounted; a forced stop cuts the console off before it.
    if ! grep -rq "System Power Off" "$state"; then
      echo "FAIL: no guest poweroff on the console"
      exit 1
    fi
  '';
}
