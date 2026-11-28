# shipping

`shipping.quote(order)` returns the shipping charge in cents for an order:

    {"postcode": "1011", "promo": None, "items": [{"cents": 2500, "kg": 1.5}]}

It looks the delivery zone up from the postcode (`zones.py`), prices the parcel from
the zone's rate card (`rates.py`) and applies a promo code if the order has one
(`promos.py`). All three are plain modules in this repository.

The rate cards are what customers are charged. A change to the charge needs a test that
prices a parcel through the real zones, rate cards and promo codes, so that a wrong card
fails it. The tests in `test_shipping.py` patch all three and pass whatever the cards say.
