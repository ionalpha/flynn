def parse_money(text):
    """Parse '1,234.50', '12' or '(12.50)' into integer cents; parentheses mean negative."""
    text = text.strip().replace(",", "")
    negative = text.startswith("(") and text.endswith(")")
    if negative:
        text = text[1:-1]
    whole, _, frac = text.partition(".")
    frac = (frac + "00")[:2]
    cents = int(whole) * 100 + int(frac)
    return -cents if negative else cents
