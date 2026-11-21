# interest

`balance_after(principal_cents, annual_rate_percent, months)` is the balance of a savings
account after `months` whole months.

Interest compounds monthly at `annual_rate_percent / 12` per month. The result is rounded
half up to the whole cent once, at the end, not after each month.
