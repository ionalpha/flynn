"""Stand-in for the acmepay SDK."""


class Card:
    def __init__(self, brand, last4):
        self.brand = brand
        self.last4 = last4


class PaymentMethod:
    def __init__(self, card):
        self.card = card


class Charge:
    def __init__(self, id, amount, currency, payment_method):
        self.id = id
        self.amount = amount
        self.currency = currency
        self.payment_method = payment_method


class _Charges:
    def list(self, month):
        return [
            Charge("ch_1", 4200, "usd", PaymentMethod(Card("visa", "4242"))),
            Charge("ch_2", 1999, "usd", PaymentMethod(Card("mastercard", "4444"))),
            Charge("ch_3", 500, "usd", PaymentMethod(Card("amex", "0005"))),
        ]

    def retrieve(self, charge_id):
        return {c.id: c for c in self.list(None)}[charge_id]


class Client:
    def __init__(self, api_key):
        self.charges = _Charges()
