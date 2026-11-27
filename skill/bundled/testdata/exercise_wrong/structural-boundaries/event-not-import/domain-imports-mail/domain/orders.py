import events
from adapters import mail

ORDERS = {}


def place_order(order):
    if order["cents"] <= 0:
        raise ValueError("an order needs a positive amount")
    ORDERS[order["id"]] = order
    mail.send(order["email"], "Order confirmed", "We have your order %s." % order["id"])


def cancel_order(order_id):
    events.publish("order_cancelled", ORDERS.pop(order_id))
