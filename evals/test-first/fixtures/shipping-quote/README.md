# quote

`quote(lines, region, coupon=None)` in `quote.py` prices an order. Every amount is an integer number of cents. It returns a dict with the keys `subtotal`, `discount`, `shipping`, `tax` and `total`.

- `lines` is a non-empty list of `(unit_cents, qty)` pairs. `unit_cents` is an int of 0 or more and `qty` is an int of 1 or more. Anything else raises `ValueError`.
- `subtotal` is the sum of `unit_cents * qty`.
- `coupon` is `None`, `"TEN"` or `"FIVE"`. Any other value raises `ValueError`. `"TEN"` takes 10% of the subtotal, rounded to the nearest cent with a tie going to the even cent (100.5 becomes 100, 101.5 becomes 102). `"FIVE"` takes 500 cents, but never more than the subtotal.
- `discounted` is `subtotal - discount`.
- `shipping` is 0 when `discounted` is at least 5000. Otherwise it is 500 for `US`, 700 for `EU` and 900 for `UK`. Any other region raises `ValueError`, also when shipping would have been free.
- `tax` is 0 for `US`. For `EU` it is 19% and for `UK` 20% of `discounted + shipping`, rounded to the nearest cent with a tie going up (`money.round_half_up`).
- `total` is `discounted + shipping + tax`.
