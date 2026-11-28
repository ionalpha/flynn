# shop

`checkout.total` prices a cart in cents. `refunds.py` works out partial refunds.
CI runs `python3 -m unittest discover`.

## Flags

Flags live in `flags.json` and are read with `flags.enabled(name)`.

- `new_checkout`: per-line bulk discounts in place of the old flat discount. On for
  everyone since March.
- `gift_notes`: still being trialled.
