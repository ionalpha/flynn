"""The price list, in cents."""

import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))


def load_prices():
    with open(os.path.join(HERE, "catalog.json"), encoding="utf-8") as f:
        return json.load(f)
