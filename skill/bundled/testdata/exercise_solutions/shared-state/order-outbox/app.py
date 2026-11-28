"""Checkout."""


def init(conn):
    """Create what is missing. Runs at every start."""
    conn.execute("CREATE TABLE IF NOT EXISTS orders (id TEXT PRIMARY KEY, total INTEGER NOT NULL)")
    conn.execute("CREATE TABLE IF NOT EXISTS outbox (id TEXT PRIMARY KEY, total INTEGER NOT NULL)")
    conn.commit()


def place_order(conn, bus, order_id, total):
    """Save an order and tell the warehouse about it."""
    with conn:
        conn.execute("INSERT INTO orders (id, total) VALUES (?, ?)", (order_id, total))
        conn.execute("INSERT INTO outbox (id, total) VALUES (?, ?)", (order_id, total))
    try:
        relay(conn, bus)
    except ConnectionError:
        pass


def relay(conn, bus):
    """Run by cron every minute."""
    for order_id, total in conn.execute("SELECT id, total FROM outbox").fetchall():
        bus.publish({"type": "order.placed", "id": order_id, "total": total})
        with conn:
            conn.execute("DELETE FROM outbox WHERE id = ?", (order_id,))
