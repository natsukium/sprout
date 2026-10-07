# Guarded so a boot whose workspace mount failed lands in $HOME instead of
# erroring every login. Remote *commands* get the same default host-side
# (sshInvocation), since a non-interactive shell never reads login init.
# Only sprout's own login user is moved: a service account entered with
# `sudo -i -u` or `su -` expects to start in its home.
{ sshUser }:
{
  environment.loginShellInit = ''
    [ "$(id -un)" = ${sshUser} ] && [ -d /workspace ] && cd /workspace
  '';
}
