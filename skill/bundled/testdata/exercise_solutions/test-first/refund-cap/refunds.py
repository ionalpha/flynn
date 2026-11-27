class Order:
    def __init__(self, paid_cents, items):
        self.paid_cents = paid_cents
        self.items = dict(items)  # sku -> price of that item before any order discount
        self.refunds = []


def refunded(order):
    return sum(order.refunds)


def refund(order, cents):
    if cents <= 0:
        raise ValueError("a refund must be positive")
    _record(order, cents)


def refund_item(order, sku):
    """Refund one returned item at its price."""
    _record(order, order.items[sku])
    del order.items[sku]


def _record(order, cents):
    if refunded(order) + cents > order.paid_cents:
        raise ValueError("refunds would exceed what was paid")
    order.refunds.append(cents)
