{ pkgs, ... }:
{
  # Drop traefik so the todo-web LoadBalancer can own port 80.
  services.k3s.disable = [ "traefik" ];

  # skaffold's docker builder talks to this daemon.
  virtualisation.docker.enable = true;

  # The bridge between builder and cluster: skaffold pushes here, k3s pulls
  # from here.
  services.dockerRegistry = {
    enable = true;
    listenAddress = "127.0.0.1";
    port = 5000;
  };

  # Without this, k3s's containerd assumes HTTPS and refuses the loopback
  # registry.
  environment.etc."rancher/k3s/registries.yaml".text = ''
    mirrors:
      "localhost:5000":
        endpoint:
          - "http://localhost:5000"
  '';

  # skaffold prefixes every built image with the registry, so skaffold.yaml and
  # the manifests can name the bare image and stay registry-agnostic.
  environment.variables.SKAFFOLD_DEFAULT_REPO = "localhost:5000";

  # skaffold shells out to kubectl, which the k3s profile already installs.
  environment.systemPackages = [ pkgs.skaffold ];
}
