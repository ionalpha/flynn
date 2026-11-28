SEPARATORS = {"de": (".", ",")}
LOCALE = "en"


def set_locale(locale):
    global LOCALE
    LOCALE = locale


def fmt(cents, currency="EUR"):
    group, point = SEPARATORS.get(LOCALE, (",", "."))
    whole, frac = divmod(abs(cents), 100)
    text = "{:,}".format(whole).replace(",", group) + point + "%02d" % frac
    return ("-" if cents < 0 else "") + text + " " + currency
