"""Taking payment for an order."""
from gateway import Timeout

ATTEMPTS = 3


def pay(gateway, order):
    """Charge under one key per order, so a retry after a lost reply cannot charge twice.

    When every attempt times out the charge may still have been made, so the gateway is
    asked before the order is called failed.
    """
    key = "order-%s" % order["id"]
    for _ in range(ATTEMPTS):
        try:
            gateway.charge(order["id"], order["cents"], key=key)
        except Timeout:
            continue
        order["state"] = "paid"
        return
    for _ in range(ATTEMPTS):
        try:
            held = gateway.status(key)
        except Timeout:
            continue
        order["state"] = "paid" if held else "failed"
        return
    order["state"] = "unknown"
