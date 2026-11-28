# orders

`order_payload(order)` builds the JSON body of the `order.created` webhook. About forty
partner integrations parse it; their side is not in this repository. PARTNERS.md is
the description they were given.

Run the tests with `python3 -m unittest discover -q`.
