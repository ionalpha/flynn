class ApiDown(Exception):
    pass


class NotFound(Exception):
    pass


class OrdersApi:
    """Stand-in for the orders service client.

    `orders` maps an order id to {"status": s, "version": n}; set `down` to make
    every read fail the way an outage does.
    """

    def __init__(self, orders=None):
        self.orders = {k: dict(v) for k, v in (orders or {}).items()}
        self.calls = []
        self.down = False

    def get(self, order_id):
        self.calls.append(order_id)
        if self.down:
            raise ApiDown("orders service unavailable")
        if order_id not in self.orders:
            raise NotFound(order_id)
        return dict(self.orders[order_id])
