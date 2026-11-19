"""Taking payment for an order."""

from payments import GatewayError


def pay(charge, amount_cents):
    try:
        return charge(amount_cents)
    except GatewayError as e:
        if not e.retryable:
            raise
        for _ in range(2):
            try:
                return charge(amount_cents)
            except GatewayError:
                pass
        return charge(amount_cents)
