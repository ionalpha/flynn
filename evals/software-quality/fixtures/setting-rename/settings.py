"""Settings of the sync job."""
import configparser
import os

DEFAULTS = {"endpoint": "https://inventory.internal", "timeout": "30", "retries": "3"}


def load(path, env=None):
    """The defaults, then the ini file, then the environment. Unknown keys are ignored."""
    env = os.environ if env is None else env
    values = dict(DEFAULTS)
    parser = configparser.ConfigParser(interpolation=None)
    parser.read(path, encoding="utf-8")
    if parser.has_section("sync"):
        values.update((k, v) for k, v in parser["sync"].items() if k in DEFAULTS)
    for key in DEFAULTS:
        name = "SYNC_" + key.upper()
        if name in env:
            values[key] = env[name]
    return values
