# docs

A small document service. `handlers.py` is what the web layer calls with the signed-in
user. A user is a dict with an `id` and a `role` (`member` or `admin`). A member reads
documents they own and documents shared with them; an admin reads everything.

Run the tests with `python3 -m unittest discover -q`.
