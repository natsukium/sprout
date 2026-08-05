# gh keeps its token in the macOS Keychain, so a mount cannot see it; the
# host script extracts it and renders the guest's hosts.yml.
#
# hostPkgs is a module arg of the *outer* VM module; the entry submodule
# below does not inherit it, so the exec default captures it from this
# function's closure instead (forced only when the exec value is read).
{ lib, hostPkgs, ... }:
{
  options.credentials.gh = lib.mkOption {
    type = lib.types.submodule [
      ../entry.nix
      {
        strategy = lib.mkDefault "materialize";
        guestPath = lib.mkDefault "/root/.config/gh/hosts.yml";
        exec = lib.mkDefault [
          (lib.getExe (
            hostPkgs.writeShellApplication {
              name = "sprout-gh-materialize";
              runtimeInputs = [ hostPkgs.gh ];
              text = ''
                token=$(gh auth token)
                printf 'github.com:\n    oauth_token: %s\n    git_protocol: https\n' "$token"
              '';
            }
          ))
        ];
      }
    ];
    default = { };
    description = "The gh Keychain token, materialized into the guest's hosts.yml.";
  };
}
