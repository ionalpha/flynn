"""The CRM tokens, kept in a file that survives between nightly runs."""

import json

PATH = "credentials.json"


def load():
    with open(PATH, encoding="utf-8") as f:
        return json.load(f)


def save(data):
    with open(PATH, "w", encoding="utf-8") as f:
        json.dump(data, f)
