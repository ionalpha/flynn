# profiles

Many web processes, on several machines, share one cache and one database.

- `db.read(user_id)` returns the row, `{"id", "name", "version"}`, and is the load we
  cache to avoid.
- `db.write(user_id, name)` returns the new row. `version` rises with every write.
- `cache.py` is the shared cache client.
