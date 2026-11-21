from money import round_half_up

TAX_PERCENT = {"US": 0, "EU": 20, "UK": 20}
SHIPPING = {"US": 500, "EU": 700, "UK": 900}
FREE_ABOVE = 5000


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
        discount = round_half_up(subtotal, 10)
    elif coupon == "FIVE":
        discount = 500
    else:
        raise ValueError("unknown coupon %r" % (coupon,))
    discounted = subtotal - discount
    shipping = 0 if discounted > FREE_ABOVE else SHIPPING.get(region, SHIPPING["US"])
    tax = round_half_up((discounted + shipping) * TAX_PERCENT.get(region, 0), 100)
    return {"subtotal": subtotal, "discount": discount, "shipping": shipping, "tax": tax,
            "total": discounted + shipping + tax}
