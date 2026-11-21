def parse_money(text):
    """Parse '1,234.50' or '12' into integer cents."""
    text = text.strip().replace(",", "")
    whole, _, frac = text.partition(".")
    frac = (frac + "00")[:2]
    return int(whole) * 100 + int(frac)
