import os
import sqlite3

HERE = os.path.dirname(os.path.abspath(__file__))
CONN = sqlite3.connect(":memory:")
with open(os.path.join(HERE, "schema.sql"), encoding="utf-8") as fh:
    CONN.executescript(fh.read())


def seed():
    CONN.execute("DELETE FROM sessions")
    CONN.execute("DELETE FROM session_archive")
    CONN.executemany(
        "INSERT INTO sessions VALUES (?, ?, ?)",
        [
            ("s1", "u1", "2026-01-01"),
            ("s2", "u2", "2026-02-20"),
            ("s3", "u1", "2025-12-15"),
            ("s4", "u3", "2026-02-27"),
        ],
    )
    CONN.commit()


seed()
