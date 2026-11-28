import events

ORDERS = {}


def place_order(order):
    if order["cents"] <= 0:
        raise ValueError("an order needs a positive amount")
    ORDERS[order["id"]] = order


def cancel_order(order_id):
    events.publish("order_cancelled", ORDERS.pop(order_id))
