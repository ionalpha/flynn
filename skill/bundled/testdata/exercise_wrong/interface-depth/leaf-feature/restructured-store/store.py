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


class Orders:
    """Context manager over the orders table, so callers stop opening and closing."""

    def __init__(self, path):
        self.path = path

    def __enter__(self):
        self.db = open_db(self.path)
        return read_rows(self.db, "orders")

    def __exit__(self, *exc):
        close_db(self.db)
