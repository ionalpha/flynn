# pricing

`store.read` is a network round trip, and the same few skus are looked up all day by a
long-running process. `store.py` is the shared storage client and does not change.
Every price change goes through `pricing.set_price` or `pricing.delete_price`; nothing
else writes the store while the process runs.

Run the tests with `python3 -m unittest discover -q`.
