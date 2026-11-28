import configparser

DEFAULTS = {
    "host": "localhost",
    "port": "25",
    "timeout_seconds": "10",
}


def load(path):
    parser = configparser.ConfigParser()
    with open(path) as f:
        parser.read_file(f)
    settings = dict(DEFAULTS)
    for key, value in parser["relay"].items():
        if key not in DEFAULTS:
            raise ValueError("unknown setting: " + key)
        settings[key] = value
    return settings
