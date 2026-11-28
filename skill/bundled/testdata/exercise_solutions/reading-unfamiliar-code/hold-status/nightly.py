UNPAID_DAYS = 7


def expire(orders, today):
    """Cancel orders that were never paid. Runs every night."""
    for o in orders:
        if o.status == "new" and today - o.placed_day > UNPAID_DAYS:
            o.status = "cancelled"
