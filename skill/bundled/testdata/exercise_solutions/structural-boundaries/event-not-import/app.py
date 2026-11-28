import events
from adapters import mail, stock

events.subscribe("order_cancelled", stock.release)


def confirm(order):
    mail.send(order["email"], "Order confirmed", "We have your order %s." % order["id"])


events.subscribe("order_placed", confirm)
