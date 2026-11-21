"""Price quotes for the order desk."""

TAX_RATE = 0.2
FREE_SHIPPING_OVER = 100.0


def quote(items, tier="standard", country="GB"):
    """Parts and total for a list of (unit_price, qty) pairs."""
    subtotal = 0.0
    units = 0
    for price, qty in items:
        subtotal += price * qty
        units += qty
    discount = 0.0
    if units >= 50:
        discount = subtotal * 0.10
    elif units >= 20:
        discount = subtotal * 0.05
    if tier == "gold":
        discount += subtotal * 0.03
    elif tier == "silver":
        discount += subtotal * 0.01
    discounted = subtotal - discount
    if discounted >= FREE_SHIPPING_OVER or units == 0:
        shipping = 0.0
    elif country == "GB":
        shipping = 4.99
    else:
        shipping = 14.99
    taxable = discounted + shipping
    tax = round(taxable * TAX_RATE, 2) if country == "GB" else 0.0
    total = round(taxable + tax, 2)
    return {
        "subtotal": round(subtotal, 2),
        "discount": round(discount, 2),
        "shipping": shipping,
        "tax": tax,
        "total": total,
    }
