# billing

Subscriptions and what they give access to. CI runs `python3 -m unittest discover`.

## Words

A subscription's `status` is `active`, `cancelled` or `terminated`.

- `cancelled`: the customer chose to leave. They keep access to the end of the period
  they paid for, and the churn report counts them.
- `terminated`: we ended it (chargeback, abuse, fraud). Access stops at once. The paid
  period stays on record in case of a dispute. It is not churn.
