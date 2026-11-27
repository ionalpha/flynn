import configparser

DEFAULTS = {
    "host": "localhost",
    "port": "25",
    "timeout_seconds": "10",
}

# Settings that no longer do anything but are still in operators' own files.
REMOVED = {"retries"}


def load(path):
    parser = configparser.ConfigParser()
    with open(path) as f:
        parser.read_file(f)
    settings = dict(DEFAULTS)
    for key, value in parser["relay"].items():
        if key in REMOVED:
            continue
        if key not in DEFAULTS:
            raise ValueError("unknown setting: " + key)
        settings[key] = value
    return settings
