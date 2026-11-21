import ipaddress
from urllib.parse import urljoin, urlparse

import transport


def fetch(url, max_redirects=3):
    first = True
    for _ in range(max_redirects + 1):
        parts = urlparse(url)
        host = parts.hostname
        if parts.scheme not in ("http", "https") or not host:
            raise ValueError("blocked destination: " + url)
        ip = transport.resolve(host)[0]
        if first:
            addr = ipaddress.ip_address(ip)
            addr = getattr(addr, "ipv4_mapped", None) or addr
            if not addr.is_global:
                raise ValueError("blocked destination: " + host)
            first = False
        status, headers, body = transport.request(ip, host, parts.path or "/")
        if status in (301, 302, 307, 308):
            url = urljoin(url, headers["Location"])
            continue
        return body
    raise ValueError("too many redirects")
