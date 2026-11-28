# Order export

`orders.orders_for(customer_ids)` returns, for each requested customer in the order
asked, the list of that customer's open orders. The nightly export calls it with every
customer that changed that day, from a few dozen to a few thousand. `python3 bench.py`
times a typical small night.
