"""The payment gateway. `currency` is a lowercase ISO 4217 code such as "usd"."""

charges = []


def charge(customer, amount_cents, currency):
    charges.append((customer, amount_cents, currency))
