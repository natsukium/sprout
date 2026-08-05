# Todo app on k3s

The same todo app and Postgres database from plain [Kubernetes
manifests](manifests/stack.yaml) on a single-node k3s cluster, with
[skaffold](https://skaffold.dev) driving the dev loop. See
[`../README.md`](../README.md) for the shared app and stack.

This is the production-shaped example: a registry to push images to, and the
app reachable the way a cluster exposes it — a `LoadBalancer` on port 80 rather
than a published container port. The [podman example](../podman/) is the
lighter everyday loop.

## Run it

`sprout up` gives you a running k3s node; you deploy the stack yourself with
skaffold, from inside the guest:

```console
$ cd examples/k3s
$ sprout up
$ sprout shell
[root@sprout-todo:/workspace]# cd examples/k3s
[root@sprout-todo:/workspace/examples/k3s]# skaffold run
```

`skaffold run` builds the app image, imports it into the cluster, and applies
the manifests once. The app pod restarts a few times while it waits for
Postgres to accept connections; that is the pod backing off, not a failure.
Once both pods are `Running`, run `sprout forward --bind 0.0.0.0 80` from the
host and open <http://localhost:80>. Binding `0.0.0.0` lets an unprivileged
process use port 80 on macOS, but also exposes the app to your network; `sprout
forward 8080:80` with <http://localhost:8080> keeps it on loopback instead.
Stop the forward with Ctrl-C and remove the example environment when finished:

```console
$ sprout delete
```

## The dev loop

Instead of `run`, use `skaffold dev` and leave it running:

```console
[root@sprout-todo:/workspace]# cd examples/k3s
[root@sprout-todo:/workspace/examples/k3s]# skaffold dev
```

Edit [`../app/main.go`](../app/main.go) on the host; the source is live in the
guest at `/workspace/examples/app`. skaffold watches it, rebuilds the image,
re-imports it, rolls the Deployment, and streams the pod logs to your
terminal. The Postgres PersistentVolumeClaim is untouched, so your todos
survive. Ctrl-C tears the deployment back down.

## How it works

The workloads are plain Kubernetes YAML in
[`manifests/stack.yaml`](manifests/stack.yaml): the Deployments, Services, and
the Postgres PersistentVolumeClaim. Nothing about them is expressed in Nix.

[`skaffold.yaml`](skaffold.yaml) is a stock config: skaffold's built-in docker
builder builds the [`Containerfile`](../app/Containerfile) and pushes the image.
It names no runtime and no registry. The plumbing that makes that reach k3s
lives in [`guest.nix`](guest.nix): a docker daemon for the build, a local
registry skaffold pushes to (`SKAFFOLD_DEFAULT_REPO` points at it), and a
`registries.yaml` that lets k3s pull from it over HTTP. skaffold rewrites the
manifest's bare `image: todo` to the tag it pushed, so build and deploy never
drift.

The app reaches Postgres through the cluster DNS name of its Service
(`todo-db`). The app's Service is a `LoadBalancer` on port 80, which k3s's
built-in servicelb binds to the node; traefik is disabled so that port is
free. Postgres uses a PersistentVolumeClaim from k3s's default local-path
provisioner, which writes under `/var` and so survives `sprout stop` / `sprout up`.

[`guest.nix`](guest.nix) is only this app's machine choices: a docker daemon,
the local registry, skaffold, and the traefik opt-out. The k3s machinery
itself — kernel modules, sysctls, `kubectl`, `KUBECONFIG`, and the
kubeconfig-loopback fix — comes from sprout's `nixosModules.k3s` profile,
imported next to it in `flake.nix`. Neither knows anything about the
manifests or the app.

## Starting it on boot instead

If you would rather `sprout up` deploy the stack with no manual step (useful for
an unattended or CI runner), add a systemd unit to `guest.nix` that runs
skaffold once after k3s is up:

```nix
systemd.services.todo-deploy = {
  wantedBy = [ "multi-user.target" ];
  after = [ "k3s.service" ];
  path = [ pkgs.skaffold pkgs.kubectl pkgs.docker ];
  environment.KUBECONFIG = "/etc/rancher/k3s/k3s.yaml";
  serviceConfig = {
    Type = "oneshot";
    RemainAfterExit = true;
    WorkingDirectory = "/workspace/examples/k3s";
    ExecStart = "skaffold run";
  };
};
```

This unit couples the guest config to the workspace path. For interactive
development, `skaffold dev` keeps machine and app config separate while
providing a live rebuild loop.

## Resources

k3s wants more than a dev container: this example requests 4 vCPU and 6 GiB
(`flake.nix`). Lowering them much risks the control plane failing to
stabilize.
