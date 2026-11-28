def pick_list(orders):
    """Ids of the orders the warehouse should pack today."""
    return [o.id for o in orders if o.status == "paid"]
