# Each backend turns the shared VM description into a guest NixOS module (its
# microvm.nix settings and assertions) and the manifest's `backend` fragment,
# naming sockets only by bare name: the host resolves each into its short
# socket directory and substitutes the absolute path for
# `socketPlaceholder name`.
{
  vfkit = import ./vfkit.nix;
  qemu = import ./qemu.nix;
}
