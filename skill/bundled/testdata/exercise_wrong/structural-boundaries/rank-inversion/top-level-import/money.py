import customers

SEPARATORS = {"de": (".", ",")}


def fmt(cents, currency="EUR", customer_id=None):
    locale = "en"
    if customer_id is not None:
        locale = customers.get(customer_id)["locale"]
    group, point = SEPARATORS.get(locale, (",", "."))
    whole, frac = divmod(abs(cents), 100)
    text = "{:,}".format(whole).replace(",", group) + point + "%02d" % frac
    return ("-" if cents < 0 else "") + text + " " + currency
