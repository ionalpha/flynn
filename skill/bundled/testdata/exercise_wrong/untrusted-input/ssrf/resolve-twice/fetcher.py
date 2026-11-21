import ipaddress
from urllib.parse import urljoin, urlparse

import transport


def _check(host):
    for raw in transport.resolve(host):
        addr = ipaddress.ip_address(raw)
        addr = getattr(addr, "ipv4_mapped", None) or addr
        if not addr.is_global:
            raise ValueError("blocked destination: " + host)


def fetch(url, max_redirects=3):
    for _ in range(max_redirects + 1):
        parts = urlparse(url)
        host = parts.hostname
        if parts.scheme not in ("http", "https") or not host:
            raise ValueError("blocked destination: " + url)
        _check(host)
        ip = transport.resolve(host)[0]
        status, headers, body = transport.request(ip, host, parts.path or "/")
        if status in (301, 302, 307, 308):
            url = urljoin(url, headers["Location"])
            continue
        return body
    raise ValueError("too many redirects")
