from money import round_half_up

TAX_PERCENT = {"US": 0, "EU": 19, "UK": 20}
SHIPPING = {"US": 500, "EU": 700, "UK": 900}
FREE_FROM = 5000


def _half_even_tenth(cents):
    whole, rest = divmod(cents, 10)
    if rest > 5 or (rest == 5 and whole % 2 == 1):
        return whole + 1
    return whole


def quote(lines, region, coupon=None):
    if not lines:
        raise ValueError("an order needs at least one line")
    for unit, qty in lines:
        if type(unit) is not int or type(qty) is not int or unit < 0 or qty < 1:
            raise ValueError("bad line %r" % ((unit, qty),))
    subtotal = sum(unit * qty for unit, qty in lines)
    if coupon is None:
        discount = 0
    elif coupon == "TEN":
        discount = _half_even_tenth(subtotal)
    elif coupon == "FIVE":
        discount = min(500, subtotal)
    else:
        raise ValueError("unknown coupon %r" % (coupon,))
    discounted = subtotal - discount
    if region not in SHIPPING:
        raise ValueError("unknown region %r" % (region,))
    shipping = 0 if discounted >= FREE_FROM else SHIPPING[region]
    tax = round_half_up((discounted + shipping) * TAX_PERCENT[region], 100)
    return {"subtotal": subtotal, "discount": discount, "shipping": shipping, "tax": tax,
            "total": discounted + shipping + tax}
