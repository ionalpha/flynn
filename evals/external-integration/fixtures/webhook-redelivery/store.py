"""A small key-value store, kept on disk."""

import json
import os

PATH = "store.json"


def _read():
    if not os.path.exists(PATH):
        return {}
    with open(PATH, encoding="utf-8") as f:
        return json.load(f)


def get(key, default=None):
    return _read().get(key, default)


def put(key, value):
    data = _read()
    data[key] = value
    with open(PATH, "w", encoding="utf-8") as f:
        json.dump(data, f)


def delete(key):
    data = _read()
    data.pop(key, None)
    with open(PATH, "w", encoding="utf-8") as f:
        json.dump(data, f)
