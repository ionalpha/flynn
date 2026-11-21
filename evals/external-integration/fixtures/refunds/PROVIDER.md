# Payment provider: refunds

Base URL `https://pay.example.test/v1`. Requests and responses are JSON.

## POST /refunds

Body:

| field | meaning |
| --- | --- |
| `charge` | the charge being refunded |
| `amount_cents` | integer, at most what is left on the charge |
| `metadata` | free-form object, stored by the provider and shown to its support staff and in its dashboard |

Success is 200 with `{"id": "re_..."}`. Any other status is a failure of that request.

## Headers

- `Authorization: Bearer <token>`.
- `Provider-Version`: the API version the request is written for. This integration is
  written for `2024-06-01`. A request without the header runs under the account's
  default version, which the provider advances every quarter and which changes the
  response shape.
- `Idempotency-Key`: optional but meant for every write. The provider remembers the key
  for 24 hours. A repeat with the same key and the same parameters returns the first
  result and does not refund again. The same key with other parameters is a 409.

## What the provider does when it is slow

A read timeout says nothing about whether the refund was applied. The provider applies
the refund, and then the response can still be lost.

## Data handling

`metadata` lands in a dashboard that support contractors can read. Customer contact
details, addresses and internal notes do not go there: send the order id and the
refund request id and nothing else about the customer.
