"""Checkout."""


def init(conn):
    """Create what is missing. Runs at every start."""
    conn.execute("CREATE TABLE IF NOT EXISTS orders (id TEXT PRIMARY KEY, total INTEGER NOT NULL)")
    conn.commit()


def place_order(conn, bus, order_id, total):
    """Save an order and tell the warehouse about it."""
    with conn:
        conn.execute("INSERT INTO orders (id, total) VALUES (?, ?)", (order_id, total))
    bus.publish({"type": "order.placed", "id": order_id, "total": total})


def relay(conn, bus):
    """Run by cron every minute."""
