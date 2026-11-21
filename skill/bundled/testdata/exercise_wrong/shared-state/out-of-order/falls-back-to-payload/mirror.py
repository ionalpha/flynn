from vendor.orders_api import ApiDown


class Mirror:
    def __init__(self):
        self.rows = {}

    def status(self, order_id):
        row = self.rows.get(order_id)
        return row["status"] if row else None


def on_order_updated(mirror, api, event):
    order = event["order"]
    try:
        current = api.get(order)
    except ApiDown:
        current = {"status": event["status"], "version": event["version"]}
    held = mirror.rows.get(order)
    if held is None or current["version"] > held["version"]:
        mirror.rows[order] = {"status": current["status"], "version": current["version"]}
