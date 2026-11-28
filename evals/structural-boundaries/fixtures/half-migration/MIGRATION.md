# Moving reports to render.py

`render.table` replaces `legacy_render.table`. Its output differs (no rule line, column
widths computed from the data), and finance and audit have people downstream who parse
the old layout, so each report moves on its own, in a change that does nothing else.

- sales: moved
- stock: moved
- finance: not moved
- payroll: not moved
- audit: not moved

`legacy_render.py` goes when the last report has moved.
