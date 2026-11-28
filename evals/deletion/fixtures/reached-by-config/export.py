import importlib
import json
import os


def run(fmt, rows):
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "formats.json")
    with open(path, encoding="utf-8") as fh:
        module, func = json.load(fh)[fmt].split(":")
    return getattr(importlib.import_module(module), func)(rows)
