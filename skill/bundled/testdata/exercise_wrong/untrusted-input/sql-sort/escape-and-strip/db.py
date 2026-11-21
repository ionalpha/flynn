import re


def find_users(conn, name, sort="name"):
    name = name.replace("'", "''")
    sort = re.sub(r"[^A-Za-z0-9_]", "", sort)
    sql = "SELECT id, name FROM users WHERE name = '%s' ORDER BY %s" % (name, sort)
    return conn.execute(sql).fetchall()
