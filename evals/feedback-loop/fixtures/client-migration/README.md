# Directory client 2.0

`client.get_user(user_id, *, include_deleted=False)` replaces
`client.fetch_user(user_id, deleted_ok=False)`, which this release removes. The new flag
is keyword-only. Every module under `app/` reads users through the client.

`cache.Cache.fetch_user` is the cache's own method and is not part of the client. Run
the tests with `python -m unittest`.
