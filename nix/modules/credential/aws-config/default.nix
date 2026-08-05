# $AWS_CONFIG_FILE can move the CLI config outside ~/.aws (XDG layouts),
# leaving the aws mount alone unable to resolve any profile. Materialize the
# host's *effective* config and point the guest at the copy; the SSO token
# cache still comes live from the ~/.aws mount. Enable alongside `aws`.
#
# hostPkgs is a module arg of the *outer* VM module; the entry submodule
# below does not inherit it, so the exec default captures it from this
# function's closure instead (forced only when the exec value is read).
{ lib, hostPkgs, ... }:
{
  options.credentials.aws-config = lib.mkOption {
    type = lib.types.submodule [
      ../entry.nix
      (
        { config, ... }:
        {
          config = {
            strategy = lib.mkDefault "materialize";
            guestPath = lib.mkDefault "/root/.config/aws/config";
            guestEnv.AWS_CONFIG_FILE = lib.mkDefault config.guestPath;
            exec = lib.mkDefault [
              (lib.getExe (
                hostPkgs.writeShellApplication {
                  name = "sprout-aws-config-materialize";
                  # Resolved at up time honoring $AWS_CONFIG_FILE. A missing
                  # file projects nothing rather than aborting the boot — the
                  # credential is opt-in.
                  text = ''
                    config="''${AWS_CONFIG_FILE:-$HOME/.aws/config}"
                    if [ -f "$config" ]; then
                      cat "$config"
                    fi
                  '';
                }
              ))
            ];
          };
        }
      )
    ];
    default = { };
    description = "The host's effective AWS CLI config, rendered at boot and selected via AWS_CONFIG_FILE.";
  };
}
