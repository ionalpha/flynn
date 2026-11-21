def find_users(conn, name, sort="name"):
    return conn.execute("SELECT id, name FROM users WHERE name = ? ORDER BY " + sort, (name,)).fetchall()
