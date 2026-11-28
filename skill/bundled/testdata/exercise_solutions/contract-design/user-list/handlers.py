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


def _encode(row):
    return base64.urlsafe_b64encode(("%s|%d" % (row["created_at"], row["id"])).encode()).decode()


def _decode(cursor):
    created_at, ident = base64.urlsafe_b64decode(cursor.encode()).decode().rsplit("|", 1)
    return created_at, int(ident)


def list_users(params):
    """A page of users, newest first, ordered by (created_at, id) so ties are stable."""
    limit = min(int(params.get("limit") or DEFAULT_LIMIT), MAX_LIMIT)
    rows = sorted(db.all_users(), key=lambda r: (r["created_at"], r["id"]), reverse=True)
    if params.get("cursor"):
        after = _decode(params["cursor"])
        rows = [r for r in rows if (r["created_at"], r["id"]) < after]
    page = rows[:limit]
    more = len(rows) > limit
    return {
        "items": [{k: r[k] for k in PUBLIC} for r in page],
        "next_cursor": _encode(page[-1]) if more else None,
    }
