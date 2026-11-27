from core import orders


def submit(db, customer_id, cents):
    try:
        orders.place(db, customer_id, cents)
    except orders.Refused as e:
        return str(e)
    return "ok"


def preview(db, customer_id, cents):
    return submit(db, customer_id, cents)
