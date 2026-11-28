from datetime import date, timedelta

import db

__all__ = ["purge_old_sessions", "active_sessions"]


def purge_old_sessions(now, days=30):
    cutoff = (date.fromisoformat(now) - timedelta(days=days)).isoformat()
    cur = db.CONN.execute(
        "INSERT INTO session_archive SELECT session_id, user_id, created_at, ? FROM sessions WHERE created_at < ?",
        (now, cutoff),
    )
    db.CONN.execute("DELETE FROM sessions WHERE created_at < ?", (cutoff,))
    db.CONN.commit()
    return cur.rowcount


def active_sessions():
    rows = db.CONN.execute("SELECT session_id FROM sessions ORDER BY created_at, session_id")
    return [r[0] for r in rows]
