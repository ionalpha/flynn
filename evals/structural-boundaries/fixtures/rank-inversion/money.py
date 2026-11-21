def fmt(cents, currency="EUR"):
    whole, frac = divmod(abs(cents), 100)
    text = "{:,}.{:02d}".format(whole, frac)
    return ("-" if cents < 0 else "") + text + " " + currency
