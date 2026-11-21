from orders import orders_for_customer


def lifetime_spend(customer_id):
    return sum(o["total_cents"] for o in orders_for_customer(customer_id))
