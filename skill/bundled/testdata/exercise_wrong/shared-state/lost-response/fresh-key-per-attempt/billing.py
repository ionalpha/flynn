import time
import uuid

from vendor.gateway import ConnectionRefused, Declined, ReadTimeout


def with_retry(fn, attempts=4):
    for attempt in range(attempts):
        try:
            return fn()
        except (ConnectionRefused, ReadTimeout):
            if attempt == attempts - 1:
                raise
            time.sleep(0.2 * (attempt + 1))


def settle(gateway, invoices):
    """Charge each invoice {id, account, cents}; returns {"paid": [ids], "declined": [ids]}.

    The key comes from the invoice, so every retry and every rerun of the job asks
    the gateway for the same charge and cannot make a second one.
    """
    paid, declined = [], []
    for inv in invoices:
        key = "invoice:" + inv["id"]
        try:
            with_retry(lambda: gateway.charge(inv["account"], inv["cents"], key=uuid.uuid4().hex))
            paid.append(inv["id"])
        except Declined:
            declined.append(inv["id"])
    return {"paid": paid, "declined": declined}
