from vendor.fmt import render


def dedupe(rows):
    """Rows with a repeated (account, day, amount) removed; the first one stays."""
    seen = set()
    out = []
    for row in rows:
        key = "|".join((str(row["account"]), str(row["day"]), str(row["amount"])))
        if key not in seen:
            seen.add(key)
            out.append(row)
    return out


def build(rows):
    return [render(row) for row in dedupe(rows)]
