import importlib
import json
import os

with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "settings.json"), encoding="utf-8") as f:
    _SETTINGS = json.load(f)

_loaded = {}


def _channel(name):
    if name not in _loaded:
        _loaded[name] = importlib.import_module(_SETTINGS["channels"][name])
    return _loaded[name]


def send(name, user, msg):
    """Deliver msg to user on the named channel; returns the delivery line."""
    return _channel(name).deliver(user, msg)
