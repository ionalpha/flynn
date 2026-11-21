# Payouts, API v1

`POST https://pay.example.com/v1/payouts` with the body `{"account": "...",
"amount_cents": 5000}` pays an amount to a connected account and returns
`{"id": "po_..."}`.

Header `Idempotency-Key`: any string up to 255 characters. A request repeated with the
same key within 24 hours returns the first request's response and creates no second
payout. The same key sent with different parameters is refused with 409. Requests
without a key each create a payout.

A request can time out after the payout was created.
