import importlib
import json
import os
import sys


def run(job):
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "jobs.json")
    with open(path, encoding="utf-8") as fh:
        target = json.load(fh)[job]
    module, func = target.split(":")
    return getattr(importlib.import_module(module), func)()


if __name__ == "__main__":
    print(run(sys.argv[1]))
