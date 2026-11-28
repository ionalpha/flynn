import json
import os

_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "config")


def _merge(base, over):
    for key, value in over.items():
        if isinstance(value, dict) and isinstance(base.get(key), dict):
            _merge(base[key], value)
        else:
            base[key] = value
    return base


def load(env=None):
    env = env or os.environ.get("APP_ENV") or "dev"
    cfg = {}
    for name in ("defaults", env):
        path = os.path.join(_DIR, name + ".json")
        if os.path.exists(path):
            with open(path, encoding="utf-8") as f:
                _merge(cfg, json.load(f))
    return cfg
