# Billing webhooks

We POST each event to your endpoint as JSON:

    {"id": "evt_81", "delivery": 1, "type": "invoice.paid",
     "data": {"account": "acct_7", "amount_cents": 4200}}

Delivery is at least once. If we get no 2xx within 10 seconds, or the connection drops, we send
the same event again: the `id` is the same and `delivery` is the next number. We keep trying for
up to three days, so a redelivery can arrive hours later, after your service has restarted.
