ZERO_DECIMAL = {"JPY", "KRW"}


def fmt_money(cents):
    """cents as a plain decimal: 123450 is '1234.50'."""
    return "%d.%02d" % divmod(cents, 100)


def money(cents, currency="USD"):
    """Currency code and amount: 123450 is 'USD 1,234.50'."""
    whole, frac = divmod(cents, 100)
    return "%s %s.%02d" % (currency, format(whole, ","), frac)


def display_amount(amount, currency):
    """Currency code and amount as the pages customers read show them.

    amount is in the currency's smallest unit. A zero-decimal currency has none below the
    unit, so 5000 JPY is 'JPY 5,000'.
    """
    if currency in ZERO_DECIMAL:
        return "%s %s" % (currency, format(amount, ","))
    return money(amount, currency)
