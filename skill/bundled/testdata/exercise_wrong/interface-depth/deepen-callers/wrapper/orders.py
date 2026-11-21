from mailer import send_mail


def notify_shipped(order):
    send_mail(order["email"], "Shipped", f"Order {order['id']} is on its way.")
