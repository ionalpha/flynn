"""Lines for the thermal receipt printer, 30 characters wide."""

from money import format_amount


def receipt_lines(sale):
    currency = sale["currency"]
    lines = [f"Amounts in {currency}".center(30)]
    for name, cents in sale["items"]:
        lines.append(f"{name:<20}{format_amount(cents):>10}")
    return lines
