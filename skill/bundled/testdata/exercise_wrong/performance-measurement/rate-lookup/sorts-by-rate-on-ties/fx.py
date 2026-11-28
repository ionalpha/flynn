"""Convert transactions to the reporting currency."""

from bisect import bisect_right


def convert(transactions, rates):
    """Return each transaction's amount in the reporting currency, in order."""
    pairs = {}
    for r in rates:
        pairs.setdefault(r["currency"], []).append((r["day"], r["rate"]))
    index = {}
    for currency, found in pairs.items():
        found.sort()
        index[currency] = ([d for d, _ in found], [v for _, v in found])
    out = []
    for t in transactions:
        days, values = index.get(t["currency"], ([], []))
        i = bisect_right(days, t["day"])
        if i == 0:
            raise LookupError("no rate for %s on %s" % (t["currency"], t["day"]))
        out.append(t["amount"] * values[i - 1])
    return out
