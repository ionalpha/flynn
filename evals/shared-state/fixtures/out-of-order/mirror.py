class Mirror:
    def __init__(self):
        self.rows = {}

    def status(self, order_id):
        row = self.rows.get(order_id)
        return row["status"] if row else None


def on_order_updated(mirror, api, event):
    """Handle {"order": id, "status": s, "version": n}, sent when an order changes."""
    mirror.rows[event["order"]] = {"status": event["status"], "version": event["version"]}
