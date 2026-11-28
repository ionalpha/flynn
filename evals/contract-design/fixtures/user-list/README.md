# admin api

Handlers for the admin console. `get_user(params)` returns one user. `list_users(params)`
is the next endpoint and has not been written: the console needs a page of users, newest
first, and keeps asking for more until it has them all.

Conventions for list endpoints, as the console's client library implements them: the
request takes an optional `cursor` and an optional `limit` (default 50, at most 100); the
response carries a `next_cursor` that is passed back as `cursor` for the next page and is
null on the last one.

The console shows each user's id, name, email, role and created_at. A user row in the
database has more than that. The admin API is internal today and goes to partner
integrators in the autumn.

Run the tests with `python3 -m unittest discover -q`.
