"""Fail when a service config lacks a key of config/defaults.ini."""
import configparser
import pathlib
import sys

want = configparser.ConfigParser()
want.read("config/defaults.ini", encoding="utf-8")
problems = []
for path in sorted(pathlib.Path("config").glob("*.ini")):
    if path.name == "defaults.ini":
        continue
    got = configparser.ConfigParser(strict=False)
    got.read(path, encoding="utf-8")
    for section in want.sections():
        for key in want[section]:
            if not got.has_option(section, key):
                problems.append("%s: [%s] %s is missing" % (path, section, key))
for line in problems:
    print(line, file=sys.stderr)
sys.exit(1 if problems else 0)
