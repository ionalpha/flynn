class Refused(Exception):
    pass


def place(db, customer_id, cents):
    customer = db["customers"][customer_id]
    if customer["blocked"]:
        raise Refused("account blocked")
    owed = sum(i["cents"] for i in db["invoices"] if i["customer"] == customer_id and not i["paid"])
    if owed + cents > customer["limit_cents"]:
        raise Refused("over credit limit")
    db["orders"].append({"customer": customer_id, "cents": cents})
    db["invoices"].append({"customer": customer_id, "cents": cents, "paid": False})
