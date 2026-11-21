import json
import os
from decimal import ROUND_HALF_EVEN, Decimal


def _percent():
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "fee_schedule.json")
    with open(path, encoding="utf-8") as fh:
        return Decimal(str(json.load(fh)["percent"]))


def fee_cents(amount_cents):
    exact = Decimal(amount_cents) * _percent() / 100
    return int(exact.quantize(Decimal(1), rounding=ROUND_HALF_EVEN))
