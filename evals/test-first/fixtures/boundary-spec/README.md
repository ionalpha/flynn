# discount

`price_after_discount(total_cents)` returns what a customer pays, in whole cents.

- Orders of 10000 cents (100.00) or more get 10% off.
- The discounted price is rounded half up to the whole cent.
- Smaller orders are charged in full.
- A negative total is a programming error and raises ValueError.
