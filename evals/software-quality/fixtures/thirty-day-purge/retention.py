"""The nightly purge. The periods follow RETENTION.md."""
import datetime

SESSION_DAYS = 90
AUDIT_DAYS = 400


def cutoff(now, days):
    return (now - datetime.timedelta(days=days)).isoformat()


def purge(conn, now):
    """Delete expired rows as of the date now; return how many of each went."""
    sessions = conn.execute("DELETE FROM sessions WHERE last_seen < ?", (cutoff(now, SESSION_DAYS),)).rowcount
    audit = conn.execute("DELETE FROM audit WHERE at < ?", (cutoff(now, AUDIT_DAYS),)).rowcount
    conn.commit()
    return {"sessions": sessions, "audit": audit}
