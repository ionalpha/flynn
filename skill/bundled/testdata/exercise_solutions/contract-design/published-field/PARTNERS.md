# order.created webhook

The body is a JSON object.

| field | type | meaning |
|---|---|---|
| id | string | the order id |
| total | number | the order total in dollars, for example 19.99 |
| currency | string | ISO 4217 code |
| total_cents | integer | the order total in the currency's minor unit, for example 1999; prefer it to total |

`total` stays, with the same meaning, for integrations that read it.
