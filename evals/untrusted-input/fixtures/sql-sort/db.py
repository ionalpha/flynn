def find_users(conn, name, sort="name"):
    """Rows (id, name) for users called name, ordered by the column sort.

    Both arguments come from the query string of a public endpoint.
    """
    sql = "SELECT id, name FROM users WHERE name = '%s' ORDER BY %s" % (name, sort)
    return conn.execute(sql).fetchall()
