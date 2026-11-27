"""Order lifecycle: new -> paid -> shipped, and cancelled."""


class Order:
    def __init__(self, order_id, placed_day, status="new"):
        self.id = order_id
        self.placed_day = placed_day
        self.status = status


def pay(order):
    if order.status != "new":
        raise ValueError("cannot pay a %s order" % order.status)
    order.status = "paid"


def ship(order):
    if order.status != "paid":
        raise ValueError("cannot ship a %s order" % order.status)
    order.status = "shipped"


def cancel(order):
    if order.status in ("shipped", "cancelled"):
        raise ValueError("cannot cancel a %s order" % order.status)
    order.status = "cancelled"
