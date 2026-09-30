# SPROUT_SSHD is set only once setpriv has armed PDEATHSIG.
if [ -z "${SPROUT_SSHD-}" ]; then
  "$@"
  exit
fi
sshd=$SPROUT_SSHD
unset SPROUT_SSHD

pid=
fired=
escalated=
escalate() {
  if [ -z "$pid" ] || [ -n "$escalated" ]; then
    return 0
  fi
  escalated=1
  # Until setsid runs there is no group, but the pid alone stops the child.
  # Neither `--` nor `-s SIG` before a negative pid: dash rejects both.
  kill -TERM "-$pid" 2>/dev/null
  kill -TERM "$pid" 2>/dev/null
  (
    sleep 5
    kill -KILL "-$pid" 2>/dev/null
    kill -KILL "$pid" 2>/dev/null
  ) </dev/null >/dev/null 2>&1 &
}
trap 'fired=1; escalate' TERM HUP

# PDEATHSIG armed after sshd-session died never fires, and a shell forked as it
# died sees init as its parent (the guest has no subreaper in between).
[ "$sshd" != 1 ] && [ "$PPID" = "$sshd" ] || exit 129
[ -z "$fired" ] || exit 129

# An async list's stdin is /dev/null unless redirected explicitly.
exec 3<&0
setsid /bin/sh -c '"$@"' sh "$@" <&3 3<&- &
pid=$!
exec 3<&-
[ -z "$fired" ] || escalate

while :; do
  wait "$pid"
  st=$?
  # A trapped signal also ends wait early.
  kill -0 "$pid" 2>/dev/null || break
done

# A leftover job still writing to the session keeps ssh open, so it stays
# watched. Only pipes and sockets identify the session, and sshd-session's end
# of a pipe reads as the same link.
glob_escape() { printf '%s' "$1" | sed 's/[][*?\\]/\\&/g'; }
session_fd() {
  case $1 in
  pipe:* | socket:*) glob_escape "$1" ;;
  *) printf '%s' "no-such-link" ;;
  esac
}
out=$(session_fd "$(readlink /proc/$$/fd/1)")
err=$(session_fd "$(readlink /proc/$$/fd/2)")
session_held() {
  find /proc/[0-9]*/fd -maxdepth 1 \( -lname "$out" -o -lname "$err" \) 2>/dev/null |
    grep -v -e "^/proc/$$/" -e "^/proc/$sshd/" | grep -q .
}
while [ -z "$fired" ] && session_held </dev/null >/dev/null 2>&1; do
  sleep 1 </dev/null >/dev/null 2>&1
done
exit "$st"
