# billing hooks

The web server calls `handlers.handle_event(event)` for each webhook and answers with the
status it returns. An exception becomes a 500.

`ledger` holds account balances. `store` is a small key-value store kept on disk.
