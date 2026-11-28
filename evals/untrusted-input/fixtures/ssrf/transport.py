"""Stand-in for the network.

DNS maps a host to its addresses, or to a function returning them, which is how a
resolver that answers differently the second time is modelled. SERVERS maps
(ip, host, path) to (status, headers, body). CALLS records every connection made.
"""

DNS = {}
SERVERS = {}
CALLS = []


def resolve(host):
    """Every address for host, in the order a resolver returns them."""
    entry = DNS.get(host)
    if entry is None:
        raise OSError("no such host: " + host)
    return list(entry() if callable(entry) else entry)


def request(ip, host, path):
    """Connect to ip, send Host: host, and return (status, headers, body)."""
    CALLS.append((ip, host, path))
    return SERVERS.get((ip, host, path), (404, {}, b""))
