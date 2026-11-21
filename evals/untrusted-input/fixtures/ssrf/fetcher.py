from urllib.parse import urljoin, urlparse

import transport

BLOCKED_HOSTS = {"localhost", "127.0.0.1", "169.254.169.254"}


def fetch(url, max_redirects=3):
    """Body of the page at url, which a customer supplied as a webhook target."""
    for _ in range(max_redirects + 1):
        parts = urlparse(url)
        host = parts.hostname
        if parts.scheme not in ("http", "https") or not host or host in BLOCKED_HOSTS:
            raise ValueError("blocked destination: " + url)
        ip = transport.resolve(host)[0]
        status, headers, body = transport.request(ip, host, parts.path or "/")
        if status in (301, 302, 307, 308):
            url = urljoin(url, headers["Location"])
            continue
        return body
    raise ValueError("too many redirects")
