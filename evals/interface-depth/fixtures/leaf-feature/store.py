"""Reading the order database. Open it, read a table, close it."""

import json


def open_db(path):
    with open(path, encoding="utf-8") as f:
        return {"tables": json.load(f), "open": True}


def read_rows(db, table):
    if not db["open"]:
        raise RuntimeError("database is closed")
    return list(db["tables"][table])


def close_db(db):
    db["open"] = False
