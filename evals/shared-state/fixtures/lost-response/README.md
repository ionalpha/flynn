# billing

`billing.settle(gateway, invoices)` charges each invoice through the payment gateway
and is run nightly, and again by hand after a crash. `vendor/gateway.py` is the
gateway client shared by every service; it cannot change. Its `charge` accepts an
optional `key`: a repeated key returns the first result and charges nothing more.
Run the tests with `python3 -m unittest discover -q`.

A charge the run could not complete must not disappear: `settle` raises, or returns
the invoice id under a key other than `paid` and `declined`.

A bug fix here ships with a test that fails on the code before the fix.
