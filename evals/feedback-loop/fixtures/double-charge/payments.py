"""Taking payment for an order."""
from gateway import Timeout

ATTEMPTS = 3


def pay(gateway, order):
    for _ in range(ATTEMPTS):
        try:
            gateway.charge(order["id"], order["cents"])
        except Timeout:
            continue
        order["state"] = "paid"
        return
    order["state"] = "failed"
