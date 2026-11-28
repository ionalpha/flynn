def parse_cents(text):
    """Cents in text, a decimal such as 12.50."""
    whole, _, frac = text.strip().partition(".")
    return int(whole) * 100 + int((frac + "00")[:2])
