_SORTS = {"id": "id", "name": "name"}


def find_users(conn, name, sort="name"):
    """Rows (id, name) for users called name, ordered by the column sort.

    Both arguments come from the query string of a public endpoint.
    """
    if sort not in _SORTS:
        raise ValueError("unsupported sort: %r" % (sort,))
    return conn.execute(
        "SELECT id, name FROM users WHERE name = ? ORDER BY " + _SORTS[sort], (name,)
    ).fetchall()
