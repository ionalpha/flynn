"""The users table, in memory."""

_ROWS = []


def reset(rows):
    _ROWS[:] = [dict(r) for r in rows]


def add_user(row):
    _ROWS.append(dict(row))


def all_users():
    """Every row, in insertion order."""
    return [dict(r) for r in _ROWS]


def get(user_id):
    for r in _ROWS:
        if r["id"] == user_id:
            return dict(r)
    return None
