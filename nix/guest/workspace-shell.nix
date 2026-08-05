# Guarded so a boot whose workspace mount failed lands in $HOME instead of
# erroring every login. Remote *commands* get the same default host-side
# (sshInvocation), since a non-interactive shell never reads login init.
{
  environment.loginShellInit = ''
    [ -d /workspace ] && cd /workspace
  '';
}
