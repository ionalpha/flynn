# checkout

`app.place_order` saves an order in SQLite and announces it on the message bus, which
the warehouse consumes. `bus.publish` raises `ConnectionError` while the broker is
unreachable.

- The process can be killed at any line, and the broker can be down for hours.
- `app.relay(conn, bus)` is run every minute by cron.
- Consumers deduplicate by order id, but the warehouse keeps those ids for a day
  only: an order already announced must not be announced again by every later run.
- `app.init` runs at every start. The production database already has `orders`.
