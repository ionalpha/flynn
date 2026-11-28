import datetime
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


def _quiet(user, msg):
    window = user.get("quiet")
    if not window or msg["priority"] == "urgent":
        return False
    at = datetime.datetime.strptime(msg["at"], "%Y-%m-%dT%H:%MZ")
    hour = (at + datetime.timedelta(minutes=user.get("utc_offset_min", 0))).hour
    start, end = window
    return start <= hour < end if start < end else hour >= start or hour < end


def send(name, user, msg):
    """Deliver msg to user on the named channel; returns the delivery line, or None while a push channel is in quiet hours."""
    if name == "mobile" and _quiet(user, msg):
        return None
    return _channel(name).deliver(user, msg)
