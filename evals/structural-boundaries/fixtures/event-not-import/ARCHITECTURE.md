# Architecture

- `domain/` holds the rules. It imports nothing from `adapters/`.
- When something outside the domain must react, the domain publishes an event
  (`events.py`) and `app.py` subscribes the adapter to it.
- Every entry point imports `app` first.
