class Refused(Exception):
    pass


def refusal(db, customer_id, cents):
    """Why place would refuse this order, or None if it would accept it."""
    customer = db["customers"][customer_id]
    if customer["blocked"]:
        return "account blocked"
    owed = sum(i["cents"] for i in db["invoices"] if i["customer"] == customer_id and not i["paid"])
    if owed + cents > customer["limit_cents"]:
        return "over credit limit"
    return None


def place(db, customer_id, cents):
    reason = refusal(db, customer_id, cents)
    if reason:
        raise Refused(reason)
    db["orders"].append({"customer": customer_id, "cents": cents})
    db["invoices"].append({"customer": customer_id, "cents": cents, "paid": False})
