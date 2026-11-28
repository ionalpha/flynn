# store

A small key-value client used by several services.

## Errors

- `Store.get(key)` raises `KeyError` when the key does not exist.
- `Store.get` and `Store.put` raise `OSError`, or a subclass such as `TimeoutError`, when the
  backend cannot be reached.

Callers retry on `OSError` and fall back to a default on `KeyError`.
