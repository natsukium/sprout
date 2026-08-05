# Todo app on podman

The todo app and its Postgres database from a plain
[`compose.yaml`](compose.yaml), run with podman. See
[`../README.md`](../README.md) for the shared app and stack.

This is the lighter everyday loop: no control plane, and the app published
straight out of the guest on 8080. The [k3s example](../k3s/) is the
production-shaped one.

## Run it

`sprout up` gives you the machine; you start the stack yourself, with the same
`podman-compose` command you would run anywhere:

```console
$ cd examples/podman
$ sprout up
$ sprout shell
[root@sprout-todo:/workspace]# cd examples/podman
[root@sprout-todo:/workspace/examples/podman]# podman-compose up --build -d
[root@sprout-todo:/workspace/examples/podman]# exit
$ sprout forward 8080
```

Open <http://localhost:8080>. Add, toggle, and delete todos; they persist
across `sprout stop` / `sprout up` because the
Postgres volume is on the guest's `/var`. After booting the guest again,
restart the existing stack without rebuilding it:

```console
[root@sprout-todo:/workspace]# cd examples/podman
[root@sprout-todo:/workspace/examples/podman]# podman-compose up -d
```

If this reports a missing `podman_default` network, the existing containers do
not have a persisted network definition. Recreate the containers once with
`podman-compose down && podman-compose up -d`. The named `todo-db` volume is
not removed, so its todos remain.

The first `up --build` pulls the Postgres and build images and builds the app
image, so it takes a few minutes. Stop the forward with Ctrl-C and remove the
example environment when finished:

```console
$ sprout delete
```

## The dev loop

Edit [`../app/main.go`](../app/main.go) on the host; the source is live in the
guest at `/workspace/examples/app`. Rebuild and restart the `web` service
inside the guest:

```console
[root@sprout-todo:/workspace]# cd examples/podman
[root@sprout-todo:/workspace/examples/podman]# podman-compose up --build -d web
```

`--build` rebuilds the app image against your edited source; the `db` service
and its volume are untouched, so your todos are still there.

## How it works

The stack is an ordinary compose file: a `db` service from the Postgres image
and a `web` service built from [`../app/Containerfile`](../app/Containerfile).
Compose gives them a shared network with DNS, so the app reaches Postgres at
the hostname `db`. Start order does not matter: the app retries the database
connection until it answers. Nothing about the stack is expressed in Nix.

The sprout Podman profile enables the rootful runtime and stores its network
definitions under `/var`, alongside Podman's containers and volumes. Podman's
rootful default is `/etc/containers/networks`; that directory is rebuilt with
the guest on every boot and would leave persisted containers referring to a
missing Compose network.

[`guest.nix`](guest.nix) adds `podman-compose`. It knows nothing about the
compose file or the app.

## Starting it on boot instead

If you would rather `sprout up` bring the stack up with no manual step (useful for
an unattended or CI runner), add a systemd unit to `guest.nix` that runs the
same command:

```nix
systemd.services.todo-stack = {
  wantedBy = [ "multi-user.target" ];
  after = [ "network-online.target" ];
  wants = [ "network-online.target" ];
  path = [ pkgs.podman-compose ];
  serviceConfig.WorkingDirectory = "/workspace/examples/podman";
  script = "podman-compose up --build";
};
```

This unit couples the guest config to the workspace path and start command.
For interactive development, the manual command keeps machine and app config
separate.
