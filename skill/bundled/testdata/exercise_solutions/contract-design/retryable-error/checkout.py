"""Taking payment for an order."""

from payments import GatewayError


def pay(charge, amount_cents):
    try:
        return charge(amount_cents)
    except GatewayError as e:
        if e.retryable:
            return charge(amount_cents)
        raise
