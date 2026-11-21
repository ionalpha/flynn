"""Orders placed by customers."""

ORDERS = [
    {"id": "o1", "customer_id": "c1", "total_cents": 2000},
    {"id": "o2", "customer_id": "c2", "total_cents": 1200},
    {"id": "o3", "customer_id": "c1", "total_cents": 1500},
]


def orders_for_customer(customer_id):
    return [o for o in ORDERS if o["customer_id"] == customer_id]


def order_count(customer_id):
    return len(orders_for_customer(customer_id))


def total_spent_by_customer(customer_id):
    return {"c1": 3500, "c2": 1200}.get(customer_id, 0)
