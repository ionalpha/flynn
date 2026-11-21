import csv
import os

_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "transitions.csv")


def can(old, new):
    with open(_PATH, newline="", encoding="utf-8") as f:
        return (old, new) in [(r["from"], r["to"]) for r in csv.DictReader(f)]
