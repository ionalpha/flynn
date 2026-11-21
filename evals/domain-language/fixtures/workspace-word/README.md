# seats

Membership and billing for the team product. `api.py` serves the member lists, `invites.py`
sends invitations, `billing.py` charges and reads plans. Everything is backed by the sqlite
schema in `schema.sql`, loaded by `db.py`. Run the tests with `python3 -m unittest discover`.
