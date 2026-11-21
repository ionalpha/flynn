from orders import notify_shipped


def run(shipped_orders):
    for order in shipped_orders:
        notify_shipped(order)
