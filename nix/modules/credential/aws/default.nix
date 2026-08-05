# Mounted writable so `aws sso login` on either side refreshes the shared
# token cache. credentials.aws.readOnly = true locks it down, enforced
# guest-side.
{ lib, ... }:
{
  options.credentials.aws = lib.mkOption {
    type = lib.types.submodule [
      ../entry.nix
      {
        strategy = lib.mkDefault "mount";
        source = lib.mkDefault "~/.aws";
        target = lib.mkDefault "/root/.aws";
        readOnly = lib.mkDefault false;
      }
    ];
    default = { };
    description = "The ~/.aws directory, mounted live so SSO refresh works on either side.";
  };
}
