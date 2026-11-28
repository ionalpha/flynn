# tracking

`shipment_payload(shipment)` builds the body of the `shipment.updated` webhook, sent each
time a shipment changes. About sixty partner integrations receive it; their side is not in
this repository. PARTNERS.md is the description they were given. `labels.py` is the label
lookup our own dashboard uses.

A shipment is a dict: `id`, `carrier`, `eta` (a date string or None), `dispatched`
(bool), `returned` (bool) and `lines`, each with `sku`, `qty` and `delivered_qty`.

Run the tests with `python3 -m unittest discover -q`.
