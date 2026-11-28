import jobqueue


def enqueue_charge(order):
    jobqueue.put(
        {
            "customer": order["customer"],
            "amount_cents": order["total_cents"],
            "currency": order.get("currency", "usd"),
        }
    )
