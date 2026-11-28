"""The audit trail."""


def record(conn, actor, action, day):
    """Append one entry. Entries are never updated."""
    conn.execute("INSERT INTO audit (actor, action, at) VALUES (?, ?, ?)", (actor, action, day.isoformat()))
    conn.commit()
