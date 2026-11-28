from core import orders


def submit(db, customer_id, cents):
    try:
        orders.place(db, customer_id, cents)
    except orders.Refused as e:
        return str(e)
    return "ok"


def preview(db, customer_id, cents):
    customer = db["customers"][customer_id]
    if customer["blocked"]:
        return "account blocked"
    owed = sum(i["cents"] for i in db["invoices"] if i["customer"] == customer_id and not i["paid"])
    if owed + cents > customer["limit_cents"]:
        return "over credit limit"
    return "ok"
