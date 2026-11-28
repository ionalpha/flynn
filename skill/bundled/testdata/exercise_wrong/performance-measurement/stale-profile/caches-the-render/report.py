from functools import lru_cache

from vendor.fmt import render as _render

render = lru_cache(maxsize=None)(lambda account, day, amount: _render({"account": account, "day": day, "amount": amount}))


def dedupe(rows):
    """Rows with a repeated (account, day, amount) removed; the first one stays."""
    seen = []
    out = []
    for row in rows:
        key = (row["account"], row["day"], row["amount"])
        if key not in seen:
            seen.append(key)
            out.append(row)
    return out


def build(rows):
    return [render(r["account"], r["day"], r["amount"]) for r in dedupe(rows)]
