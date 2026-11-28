# Architecture

- `core/` decides. `web.py` presents.
- A business rule is written once, in `core/`. `web.py` asks `core/` and never reads a
  customer's or an invoice's fields to decide anything, so the page cannot disagree
  with what happens on submit.
