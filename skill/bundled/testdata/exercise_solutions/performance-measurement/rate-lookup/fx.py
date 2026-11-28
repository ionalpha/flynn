"""Convert transactions to the reporting currency."""

from bisect import bisect_right


def convert(transactions, rates):
    """Return each transaction's amount in the reporting currency, in order."""
    latest = {}
    for r in rates:
        latest.setdefault(r["currency"], {})[r["day"]] = r["rate"]
    index = {}
    for currency, by_day in latest.items():
        days = sorted(by_day)
        index[currency] = (days, [by_day[d] for d in days])
    out = []
    for t in transactions:
        days, values = index.get(t["currency"], ([], []))
        i = bisect_right(days, t["day"])
        if i == 0:
            raise LookupError("no rate for %s on %s" % (t["currency"], t["day"]))
        out.append(t["amount"] * values[i - 1])
    return out
