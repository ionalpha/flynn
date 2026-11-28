# Architecture

Four modules, lowest first: `money`, then `customers` and `pricing`, then `invoices`.
A module imports only from modules below it. `money` is the primitive everything else
formats and adds with, so it knows nothing about customers, prices or invoices.

`tools/check_imports.py` holds the ranks and runs in CI.
