"""Purchase orders: what a customer asked for, before payment."""

from dataclasses import dataclass


@dataclass
class PurchaseOrder:
    id: str
    customer_id: str
    lines: list


def purchase_order_total(purchase_order, prices):
    return sum(prices[sku] * qty for sku, qty in purchase_order.lines)
