# shipping

`shipping.quote(order)` returns the shipping charge in cents for an order:

    {"postcode": "1011", "promo": None, "items": [{"cents": 2500, "kg": 1.5}]}

It looks the delivery zone up from the postcode (`zones.py`), prices the parcel from
the zone's rate card (`rates.py`) and applies a promo code if the order has one
(`promos.py`). All three are plain modules in this repository.
