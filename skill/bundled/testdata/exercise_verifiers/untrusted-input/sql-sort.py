import sqlite3
import sys

sys.path.insert(0, ".")
from db import find_users


def fresh():
    c = sqlite3.connect(":memory:")
    c.executescript(
        "CREATE TABLE users (id INTEGER, name TEXT, email TEXT);"
        "INSERT INTO users VALUES (1, 'ann', 'a1@x.test'), (2, 'bob', 'b@x.test'),"
        " (3, 'ann', 'a2@x.test'), (4, 'o''brien', 'o@x.test');"
    )
    return c


fails = []
c = fresh()
if find_users(c, "bob") != [(2, "bob")]:
    fails.append("a plain lookup changed")
if find_users(c, "ann", sort="id") != [(1, "ann"), (3, "ann")]:
    fails.append("sort by id changed")
if find_users(c, "o'brien") != [(4, "o'brien")]:
    fails.append("a name containing a quote no longer matches")

# A hostile name is data: it matches nobody and nothing else comes back.
for name in ("x' OR '1'='1", "x' UNION SELECT id, email FROM users --", "ann'; DROP TABLE users; --"):
    try:
        rows = find_users(c, name)
    except Exception as e:
        fails.append("a hostile name raised instead of matching nobody: %r (%r)" % (name, e))
        continue
    if rows != []:
        fails.append("name %r returned %r" % (name, rows))

# A hostile sort is not a column: it is refused, and not repaired into one that works.
for sort in ("(SELECT 1)", "name;--", "name, (SELECT 1)"):
    try:
        rows = find_users(c, "ann", sort=sort)
    except Exception:
        continue
    fails.append("sort %r was accepted and returned %r" % (sort, rows))
if c.execute("SELECT count(*) FROM users").fetchone()[0] != 4:
    fails.append("the users table was changed")
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
