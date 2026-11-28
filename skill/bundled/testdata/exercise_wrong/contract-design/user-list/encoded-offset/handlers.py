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
    start = 0
    if params.get("cursor"):
        start = int(base64.urlsafe_b64decode(params["cursor"].encode()).decode().split(":")[1])
    rows = sorted(db.all_users(), key=lambda r: (r["created_at"], r["id"]), reverse=True)
    page = rows[start : start + limit]
    more = start + limit < len(rows)
    nxt = base64.urlsafe_b64encode(("o:%d" % (start + limit)).encode()).decode()
    return {"items": [{k: r[k] for k in PUBLIC} for r in page], "next_cursor": nxt if more else None}
