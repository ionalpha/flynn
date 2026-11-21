# inventory

Checkout workers run as separate processes and call inventory.reserve when an order
is placed. store.py is the client for the stock database, shared by every service;
it cannot change. Run the tests with `python3 -m unittest discover -q`.
