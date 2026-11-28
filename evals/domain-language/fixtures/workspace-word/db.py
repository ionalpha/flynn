import os
import sqlite3

HERE = os.path.dirname(os.path.abspath(__file__))
CONN = sqlite3.connect(":memory:")
with open(os.path.join(HERE, "schema.sql"), encoding="utf-8") as fh:
    CONN.executescript(fh.read())
CONN.executemany(
    "INSERT INTO workspaces VALUES (?, ?, ?)",
    [("w1", "Acme Design", 5), ("w2", "Brightside", 3)],
)
CONN.executemany(
    "INSERT INTO members VALUES (?, ?, ?, ?)",
    [
        ("m1", "w1", "ada@example.test", 1),
        ("m2", "w1", "berta@example.test", 1),
        ("m3", "w1", "cleo@example.test", 1),
        ("m4", "w1", "dev@example.test", 0),
        ("m5", "w2", "eli@example.test", 1),
        ("m6", "w2", "fay@example.test", 1),
        ("m7", "w2", "gus@example.test", 1),
    ],
)
CONN.commit()
