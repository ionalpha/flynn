"""Client for the directory service, vendored from the platform team. Do not edit."""
import time


def canonical(name):
    """The directory's spelling of a name. The real service takes about 200 ms a call."""
    time.sleep(0.2)
    return name.strip()
