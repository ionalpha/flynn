SEPARATORS = {"de": (".", ",")}


def fmt(cents, currency="EUR", locale="en"):
    group, point = SEPARATORS.get(locale, (",", "."))
    whole, frac = divmod(abs(cents), 100)
    text = "{:,}".format(whole).replace(",", group) + point + "%02d" % frac
    return ("-" if cents < 0 else "") + text + " " + currency
