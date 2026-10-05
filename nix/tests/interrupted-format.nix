# A var.img left without a filesystem, as a first boot killed mid-mkfs
# leaves it, must not stop the next boot from mounting /var: the image is
# set aside rather than booted, and a fresh one is formatted.
{ ... }:
{
  vm = { };

  testScript = ''
    up a
    dir=$(instance_dir a)
    sprout stop --instance a
    rm "$dir/var.img"
    # The state mke2fs leaves until its last write: sized, superblock zeroed.
    dd if=/dev/zero of="$dir/var.img" bs=1M count=0 seek=1024 2>/dev/null

    sprout start --instance a
    guest a 'mountpoint -q /var && touch /var/lib/after-recovery'
    ls "$dir"/var.img.unformatted-* >/dev/null || fail "the unformatted image was not set aside"
  '';
}
