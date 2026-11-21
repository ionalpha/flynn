CUSTOMERS = {
    "c1": {"name": "Ada", "locale": "en"},
    "c2": {"name": "Berta", "locale": "de"},
    "c3": {"name": "Chen", "locale": "zh"},
}


def get(customer_id):
    return CUSTOMERS[customer_id]
