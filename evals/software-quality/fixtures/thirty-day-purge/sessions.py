"""Login sessions."""


def touch(conn, user, day):
    """Record that user was seen on day (a date)."""
    conn.execute("INSERT INTO sessions (user, last_seen) VALUES (?, ?)", (user, day.isoformat()))
    conn.commit()
