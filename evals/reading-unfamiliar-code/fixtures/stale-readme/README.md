# billing

Charges and refunds for orders.

## Ledger

`ledger.post(account, amount, memo)` appends an entry to the ledger. `amount` is in dollars
as a float, for example `ledger.post("acct-1", 12.50, "top-up")`.
