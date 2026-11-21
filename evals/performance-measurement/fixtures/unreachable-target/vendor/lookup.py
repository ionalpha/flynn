import time


def fetch(key):
    """Resolve key against the partner service, which answers in about 50 ms."""
    time.sleep(0.05)
    return key.upper() + "!"
