"""Admin API handlers."""

import db

PUBLIC = ("id", "name", "email", "role", "created_at")


def get_user(params):
    row = db.get(params["id"])
    if row is None:
        return {"error": "not_found"}
    return {"user": {k: row[k] for k in PUBLIC}}


def list_users(params):
    raise NotImplementedError("not written yet")
