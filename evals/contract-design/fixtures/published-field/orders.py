"""The webhook body partners receive for a new order."""


def order_payload(order):
    return {
        "id": order["id"],
        "total": order["total_cents"] / 100,
        "currency": order["currency"],
    }
