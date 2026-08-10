"""Daily settlement report."""

from money import parse_amount


def settlement_total(rows):
    return sum(parse_amount(row["amount"]) for row in rows)


def format_total(rows):
    cents = settlement_total(rows)
    return "{}.{:02d}".format(cents // 100, cents % 100)
