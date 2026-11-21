class Mirror:
    def __init__(self):
        self.rows = {}

    def status(self, order_id):
        row = self.rows.get(order_id)
        return row["status"] if row else None


def on_order_updated(mirror, api, event):
    """Handle {"order": id, "status": s, "version": n}, sent when an order changes.

    The event only says that the order changed: it can be old, repeated or wrong,
    and the one that mattered may never arrive. The service holds the current
    order, so read it, and keep the row unless what was read is newer. An outage
    raises ApiDown for the queue to redeliver and leaves the row as it is.
    """
    order = event["order"]
    current = api.get(order)
    held = mirror.rows.get(order)
    if held is None or current["version"] > held["version"]:
        mirror.rows[order] = {"status": current["status"], "version": current["version"]}
