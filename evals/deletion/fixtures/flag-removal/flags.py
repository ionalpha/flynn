import json
import os


def enabled(name):
    with open(os.path.join(os.path.dirname(__file__), "flags.json")) as f:
        return json.load(f)[name]
