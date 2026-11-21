from mailer import HOST, Mailer


def notify_shipped(order):
    m = Mailer(HOST)
    m.connect()
    m.send(order["email"], "Shipped", f"Order {order['id']} is on its way.")
