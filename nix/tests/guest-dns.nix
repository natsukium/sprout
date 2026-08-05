# Wildcard names resolve in the guest, and they do so through a resolver whose
# address still means the resolver once /etc/resolv.conf is copied into another
# network namespace.
{ ... }:
{
  vm = {
    # Not a `.localhost` domain: nss answers those before any query is sent,
    # so a wildcard under one would pass even with no resolver at all.
    dns.wildcardDomains = [ "dev.test" ];
  };

  testScript = ''
    up dns

    guest dns 'getent hosts app.dev.test' | grep -q '^127\.0\.0\.1'
    guest dns 'getent hosts a.b.app.dev.test' | grep -q '^127\.0\.0\.1'

    # A container inherits this file into a namespace of its own, where a
    # loopback nameserver would address the container.
    guest dns 'grep -c "^nameserver" /etc/resolv.conf' | grep -qv '^0$'
    if guest dns 'grep "^nameserver 127\." /etc/resolv.conf'; then
      echo "resolv.conf names a loopback resolver" >&2
      exit 1
    fi
  '';
}
