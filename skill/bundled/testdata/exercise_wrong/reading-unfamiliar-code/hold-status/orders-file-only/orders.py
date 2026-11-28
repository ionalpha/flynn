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
    if order.status in ("shipped", "cancelled", "on_hold"):
        raise ValueError("cannot cancel a %s order" % order.status)
    order.status = "cancelled"


def hold(order):
    if order.status not in ("new", "paid"):
        raise ValueError("cannot hold a %s order" % order.status)
    order.held_from = order.status
    order.status = "on_hold"


def release(order):
    if order.status != "on_hold":
        raise ValueError("order is not on hold")
    order.status = order.held_from
