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
    limit = min(int(params.get("limit") or DEFAULT_LIMIT), MAX_LIMIT)
    start = int(params.get("cursor") or 0)
    rows = sorted(db.all_users(), key=lambda r: (r["created_at"], r["id"]), reverse=True)
    page = rows[start : start + limit]
    more = start + limit < len(rows)
    return {"items": [{k: r[k] for k in PUBLIC} for r in page], "next_cursor": str(start + limit) if more else None}
