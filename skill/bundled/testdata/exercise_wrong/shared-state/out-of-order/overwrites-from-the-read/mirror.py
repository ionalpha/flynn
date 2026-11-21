class Mirror:
    def __init__(self):
        self.rows = {}

    def status(self, order_id):
        row = self.rows.get(order_id)
        return row["status"] if row else None


def on_order_updated(mirror, api, event):
    order = event["order"]
    current = api.get(order)
    mirror.rows[order] = {"status": current["status"], "version": current["version"]}
