# events

`parse_event(data)` reads one decoded JSON event from the payment provider's feed.
The provider documents the fields id, status and amount_cents, and reserves the right
to add fields and statuses.

Run the tests with `python3 -m unittest discover -q`.
