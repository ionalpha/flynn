"""Price quotes for the order desk."""

TAX_RATE = 0.2
FREE_SHIPPING_OVER = 100.0


def _subtotal_and_units(items):
    subtotal = 0.0
    units = 0
    for price, qty in items:
        subtotal += price * qty
        units += qty
    return subtotal, units


def _discount(subtotal, units, tier):
    discount = 0.0
    if units >= 50:
        discount = subtotal * 0.10
    elif units >= 20:
        discount = subtotal * 0.05
    if tier == "gold":
        discount += subtotal * 0.03
    elif tier == "silver":
        discount += subtotal * 0.01
    return discount


def _shipping(discounted, units, country):
    if discounted >= FREE_SHIPPING_OVER or units == 0:
        return 0.0
    if country == "GB":
        return 4.99
    return 14.99


def _tax(taxable, country):
    return round(taxable * TAX_RATE, 2) if country == "GB" else 0.0


def quote(items, tier="standard", country="GB"):
    """Parts and total for a list of (unit_price, qty) pairs."""
    subtotal, units = _subtotal_and_units(items)
    discount = _discount(subtotal, units, tier)
    discounted = subtotal - discount
    shipping = _shipping(discounted, units, country)
    taxable = discounted + shipping
    tax = _tax(taxable, country)
    return {
        "subtotal": round(subtotal, 2),
        "discount": round(discount, 2),
        "shipping": shipping,
        "tax": tax,
        "total": round(taxable + tax, 2),
    }
