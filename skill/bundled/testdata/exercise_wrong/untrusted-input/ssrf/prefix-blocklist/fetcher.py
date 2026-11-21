from urllib.parse import urljoin, urlparse

import transport

PRIVATE_PREFIXES = ("10.", "169.254.", "::1", "::ffff:")


def _address(host):
    answers = transport.resolve(host)
    for raw in answers:
        if raw.startswith(PRIVATE_PREFIXES):
            raise ValueError("blocked destination: %s resolves to %s" % (host, raw))
    return answers[0]


def fetch(url, max_redirects=3):
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
