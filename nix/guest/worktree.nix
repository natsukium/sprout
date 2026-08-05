# A linked worktree's git data lives in the main repository, which arrives as
# its own share; this unit makes the two host paths git baked into the checkout
# exist in the guest.
{ gitCommonMount }:
{ lib, pkgs, ... }:
{
  systemd.services.sprout-worktree-git = {
    description = "Resolve a linked git worktree's admin directory";
    wantedBy = [ "multi-user.target" ];
    # `up` reports the VM ready as soon as sshd answers, so anything
    # ordered after it could be raced by the first guest command.
    before = [ "sshd.service" ];
    unitConfig.RequiresMountsFor = [
      "/workspace"
      gitCommonMount
    ];
    path = [ pkgs.coreutils ];
    serviceConfig = {
      Type = "oneshot";
      RemainAfterExit = true;
      ExecStart = "${pkgs.writeShellScript "sprout-worktree-git" (builtins.readFile ./worktree-git.sh)} /workspace ${gitCommonMount}";
    };
  };
  # The shared common dir also exposes every *other* worktree's registration,
  # none of which resolve in this guest, so `git gc --auto`'s prune would
  # delete them host-side once expired.
  environment.etc.gitconfig = lib.mkDefault {
    text = ''
      [gc]
      	worktreePruneExpire = never
    '';
  };
}
