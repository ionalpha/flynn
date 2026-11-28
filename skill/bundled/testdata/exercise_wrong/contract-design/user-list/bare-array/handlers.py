"""Admin API handlers."""

import base64

import db

PUBLIC = ("id", "name", "email", "role", "created_at")
DEFAULT_LIMIT = 50
MAX_LIMIT = 100


def get_user(params):
    row = db.get(params["id"])
    if row is None:
        return {"error": "not_found"}
    return {"user": {k: row[k] for k in PUBLIC}}


def list_users(params):
    rows = sorted(db.all_users(), key=lambda r: (r["created_at"], r["id"]), reverse=True)
    return rows[: int(params.get("limit") or DEFAULT_LIMIT)]
