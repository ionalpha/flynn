import ipaddress
from urllib.parse import urljoin, urlparse

import transport


def _address(host):
    """The one address to dial for host, or ValueError if any answer is not public."""
    found = []
    for raw in transport.resolve(host):
        addr = ipaddress.ip_address(raw)
        addr = getattr(addr, "ipv4_mapped", None) or addr
        if not addr.is_global:
            raise ValueError("blocked destination: %s resolves to %s" % (host, raw))
        found.append(raw)
    if not found:
        raise ValueError("blocked destination: %s does not resolve" % host)
    return found[0]


def fetch(url, max_redirects=3):
    """Body of the page at url, which a customer supplied as a webhook target."""
    for _ in range(max_redirects + 1):
        parts = urlparse(url)
        host = parts.hostname
        if parts.scheme not in ("http", "https") or not host:
            raise ValueError("blocked destination: " + url)
        ip = _address(host)
        status, headers, body = transport.request(ip, host, parts.path or "/")
        if status in (301, 302, 307, 308):
            url = urljoin(url, headers["Location"])
            continue
        return body
    raise ValueError("too many redirects")
