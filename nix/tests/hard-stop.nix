# A guest process that ignores SIGTERM holds a graceful stop until systemd or
# the host gives up on it; `--hard` must not wait for it at all, on `stop` and
# on `delete`.
{ ... }:
{
  vm = { };

  testScript = ''
    term_deaf() {
      guest "$1" 'systemd-run -E PATH=/run/current-system/sw/bin --unit=term-deaf sh -c "trap \"\" TERM; sleep 3600"'
    }
    # Well under the ~30s a graceful stop spends on the same unit.
    within() {
      local limit=$1 start
      shift
      start=$(date +%s)
      "$@"
      local took=$(($(date +%s) - start))
      echo "$* took ''${took}s"
      if [ "$took" -gt "$limit" ]; then
        echo "FAIL: expected at most ''${limit}s"
        exit 1
      fi
    }

    up hard
    term_deaf hard
    # Written without a sync of its own: only the guest sync --hard runs first
    # gets it to the disk before the power cut.
    guest hard 'echo survived > /var/lib/hard-stop-sentinel'
    within 15 sprout stop --hard --instance hard
    if sprout status --instance hard --json | grep -q '"state": *"running"'; then
      echo "FAIL: instance still running after stop --hard"
      exit 1
    fi

    up hard
    if [ "$(guest hard 'cat /var/lib/hard-stop-sentinel')" != survived ]; then
      echo "FAIL: a write to /var before stop --hard was lost"
      exit 1
    fi
    term_deaf hard
    within 15 sprout delete --force --hard --instance hard
  '';
}
