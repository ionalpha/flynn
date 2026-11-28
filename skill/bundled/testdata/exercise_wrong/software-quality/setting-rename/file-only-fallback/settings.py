"""Settings of the sync job."""
import configparser
import os

DEFAULTS = {"endpoint": "https://inventory.internal", "timeout_seconds": "30", "retries": "3"}
# Hosts keep their sync.ini and service unit across upgrades, so old names are still read.
RENAMED = {"timeout": "timeout_seconds"}


def load(path, env=None):
    """The defaults, then the ini file, then the environment. Unknown keys are ignored."""
    env = os.environ if env is None else env
    values = dict(DEFAULTS)
    parser = configparser.ConfigParser(interpolation=None)
    parser.read(path, encoding="utf-8")
    if parser.has_section("sync"):
        section = parser["sync"]
        values.update((new, section[old]) for old, new in RENAMED.items() if old in section)
        values.update((k, v) for k, v in section.items() if k in DEFAULTS)
    for key in DEFAULTS:
        name = "SYNC_" + key.upper()
        if name in env:
            values[key] = env[name]
    return values
