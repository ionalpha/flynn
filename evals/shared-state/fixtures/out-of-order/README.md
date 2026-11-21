# order mirror

The warehouse keeps a read-only copy of order status in a `Mirror`. The orders service
publishes an event whenever an order changes and `on_order_updated(mirror, api, event)`
handles each one. The queue delivers events at least once, in any order, and
occasionally drops one. `api` is the orders service client (`vendor/orders_api.py`,
shared by every service, it cannot change): `api.get(order_id)` returns the current
order, `{"status": ..., "version": n}`, where the version rises with every change.
Reads can come from a replica that is a moment behind.

Run the tests with `python3 -m unittest discover -q`.
