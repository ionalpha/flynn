# shipment.updated webhook

The body is a JSON object. Ignore any field you do not recognise; more may be added.

| field | type | meaning |
|---|---|---|
| id | string | the shipment id |
| status | string | one of `pending`, `in_transit`, `delivered`, `returned` |
| carrier | string | carrier code |
| eta | string or null | expected delivery date, `YYYY-MM-DD` |

Most partners route each shipment on `status`.
