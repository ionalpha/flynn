class Order:
    def __init__(self, paid_cents, items):
        self.paid_cents = paid_cents
        self.items = dict(items)  # sku -> price of that item before any order discount
        self.refunds = []


def refunded(order):
    return sum(order.refunds)


def refund(order, cents):
    if cents <= 0 or cents > order.paid_cents:
        raise ValueError("a refund must be positive")
    order.refunds.append(cents)


def refund_item(order, sku):
    """Refund one returned item at its price."""
    order.refunds.append(order.items.pop(sku))
