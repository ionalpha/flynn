"""Convert transactions to the reporting currency."""


def rate_on(rates, currency, day):
    """The rate in force for currency on day: the latest one dated on or before it."""
    best_day, best = None, None
    for r in rates:
        if r["currency"] == currency and r["day"] <= day:
            if best_day is None or r["day"] >= best_day:
                best_day, best = r["day"], r["rate"]
    if best is None:
        raise LookupError("no rate for %s on %s" % (currency, day))
    return best


def convert(transactions, rates):
    """Return each transaction's amount in the reporting currency, in order."""
    return [t["amount"] * rate_on(rates, t["currency"], t["day"]) for t in transactions]
