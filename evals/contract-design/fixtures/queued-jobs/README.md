# Charging

`api.enqueue_charge(order)` puts a job on the queue. A worker takes jobs off it and
`worker.handle(job)` charges the card through `gateway.charge(customer, amount_cents, currency)`.

## Operations

Deploys are rolling: for a while old and new instances of both the API and the worker run
side by side on the same queue. A job that fails is retried, so it can sit in the queue for
up to a day. Until now every price has been in US dollars.
