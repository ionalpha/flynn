# order.created webhook

The body is a JSON object.

| field | type | meaning |
|---|---|---|
| id | string | the order id |
| total | number | the order total in dollars, for example 19.99 |
| currency | string | ISO 4217 code |

`total` stays, with the same meaning, for integrations that read it.

A later release adds total_cents, the total in integer cents.
