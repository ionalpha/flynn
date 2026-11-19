import time


def canonical(host):
    """The canonical form of host. The resolver behind it takes about 20 ms a call."""
    time.sleep(0.02)
    return host.strip().lower().rstrip(".")
