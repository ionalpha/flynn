"""Taking payment for an order."""

from payments import GatewayError


def pay(charge, amount_cents):
    try:
        return charge(amount_cents)
    except GatewayError as e:
        if "taking longer than usual" in str(e):
            return charge(amount_cents)
        raise
