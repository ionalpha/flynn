# Checkout payments

`payments.pay` takes payment for an order through the card gateway. The gateway times
out now and then, and a timeout does not say whether the charge was made.

A customer is charged for an order at most once. `order["state"]` says what happened:
`paid` when the gateway holds the charge, `failed` when it holds none, and `unknown`
only when the gateway could not be asked. Run the tests with `python -m unittest`.
