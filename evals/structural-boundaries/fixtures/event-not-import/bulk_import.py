import app  # noqa: F401
from domain import orders


def run(rows):
    for order_id, email, cents in rows:
        orders.place_order({"id": order_id, "email": email, "cents": cents})
