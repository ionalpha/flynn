def parse_money(text):
    """Parse '12.50' or '(12.50)' into integer cents; parentheses mean negative."""
    text = text.strip()
    negative = text.startswith("(") and text.endswith(")")
    if negative:
        text = text[1:-1]
    whole, _, frac = text.partition(".")
    frac = (frac + "00")[:2]
    cents = int(whole) * 100 + int(frac)
    return -cents if negative else cents
