# Emulate a foreign architecture

On Apple Silicon the guest is `aarch64-linux`, so a binary or container
image built only for `x86_64-linux` does not run natively. The guest kernel
can run such binaries through QEMU user emulation. Declare the emulated
systems in the VM definition:

```nix
sprout.vms.dev.modules = [
  { boot.binfmt.emulatedSystems = [ "x86_64-linux" ]; }
];
```

This makes direct execution work: an `x86_64-linux` binary on the guest's
filesystem runs, at emulation speed rather than native speed. That covers the
occasional prebuilt tool, but not containers.

## Containers need a static emulator

With the declaration above, the kernel resolves the registered emulator
path at execution time, in the mount namespace of the process running the
foreign binary. The emulator lives in the guest's Nix store, which a
container's filesystem does not contain, so an amd64 container entrypoint
fails with `no such file or directory` naming a binary that plainly exists
in the image.

`preferStaticEmulators` switches the registration to a static QEMU with
the binfmt `F` flag, which the kernel opens once at registration time and
keeps open, so no path lookup happens inside the container:

```nix
sprout.vms.dev.modules = [
  {
    boot.binfmt = {
      emulatedSystems = [ "x86_64-linux" ];
      preferStaticEmulators = true;
    };
  }
];
```

Verify from inside the guest before relying on it:

```console
$ docker run --rm --platform linux/amd64 busybox uname -m
x86_64
```

## Keep emulation scoped

Declaring `emulatedSystems` also adds those systems to the guest Nix's
`extra-platforms` (the NixOS default), so with `writableStore` a
`nix build` for `x86_64-linux` inside the guest runs under slow local
emulation instead of being handed to a remote native builder. Set
`boot.binfmt.addEmulatedSystemsToNixSandbox = false` to keep running
foreign binaries locally while leaving their builds to remote builders,
and keep the whole block out of VM definitions that never run foreign
binaries.
