"""Fail when a service config drifts from config/defaults.ini.

Every key of the defaults must be set to a non-empty value in every service config,
under the same section and spelling, and a config sets no key the defaults do not list.
"""
import configparser
import pathlib
import sys


def load(path):
    parser = configparser.ConfigParser(interpolation=None, strict=False)
    parser.optionxform = str
    parser.read(path, encoding="utf-8")
    return {s: dict(parser[s]) for s in parser.sections()}


want = load("config/defaults.ini")
problems = []
for path in sorted(pathlib.Path("config").glob("*.ini")):
    if path.name == "defaults.ini":
        continue
    got = load(path)
    for section, keys in want.items():
        for key in keys:
            value = got.get(section, {}).get(key)
            if value is None:
                problems.append("%s: [%s] %s is missing" % (path, section, key))
            elif not value.strip():
                problems.append("%s: [%s] %s is empty" % (path, section, key))
    for section, keys in got.items():
        for key in keys:
            if key not in want.get(section, {}):
                problems.append("%s: [%s] %s is not in defaults.ini" % (path, section, key))
for line in problems:
    print(line, file=sys.stderr)
sys.exit(1 if problems else 0)
