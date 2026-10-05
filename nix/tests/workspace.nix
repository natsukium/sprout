# The workspace share in both directions, and a linked worktree whose .git
# file points at a common directory outside its own toplevel: git inside the
# guest must resolve it, and a commit made there must land in the host clone.
{ ... }:
{
  vm = {
    workspace = true;
    modules = [
      (
        { pkgs, ... }:
        {
          environment.systemPackages = [ pkgs.git ];
        }
      )
    ];
  };

  testScript = ''
    git init -q -b main repo
    cd repo
    git -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m init
    echo from-host >host-file
    up main

    test "$(guest main 'cat host-file')" = from-host
    guest main 'echo from-guest >guest-file'
    test "$(cat guest-file)" = from-guest
    test -O guest-file || fail "a guest write is not owned by the host user"

    git worktree add -q ../linked -b feature
    cd ../linked
    up feature
    guest feature 'git status --short >/dev/null'
    guest feature 'git -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m from-guest'
    test "$(git -C ../repo log -1 --format=%s feature)" = from-guest
  '';
}
