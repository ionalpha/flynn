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
    rows = sorted(db.all_users(), key=lambda r: (r["created_at"], r["id"]), reverse=True)
    if params.get("cursor"):
        after = base64.urlsafe_b64decode(params["cursor"].encode()).decode()
        rows = [r for r in rows if r["created_at"] < after]
    page = rows[:limit]
    nxt = base64.urlsafe_b64encode(page[-1]["created_at"].encode()).decode() if len(rows) > limit else None
    return {"items": [{k: r[k] for k in PUBLIC} for r in page], "next_cursor": nxt}
