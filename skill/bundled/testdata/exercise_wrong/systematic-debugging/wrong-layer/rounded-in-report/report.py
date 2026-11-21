"""Daily settlement report."""


def settlement_total(rows):
    return sum(round(float(row["amount"]) * 100) for row in rows)


def format_total(rows):
    cents = settlement_total(rows)
    return "{}.{:02d}".format(cents // 100, cents % 100)
