# fees

Card fees for checkout. `pricing.fee_cents(amount_cents)` returns the fee in whole cents,
rounded half up, at the percentage in `fee_schedule.json`. `receipt.py` prints what the
customer is charged. CI runs `python3 -m unittest discover` and is red.
