{ pkgs, ... }:
{
  environment.systemPackages = [ pkgs.podman-compose ];
}
