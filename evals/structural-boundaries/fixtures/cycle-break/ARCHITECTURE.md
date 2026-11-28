# Architecture

`purchasing` is the bottom: it records requests to buy and knows nothing else. `stock`
sits on it and owns on-hand counts and reorder levels. `orders` and `warehouse` sit on
`stock`: orders reserves for customers, warehouse moves stock between sites.

Imports point down. `test_architecture.py` fails when the top-level imports of these
modules form a cycle.
