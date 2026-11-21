# payments

`payments.gateway_error(kind)` builds the error the gateway client raises; the client
calls it with "timeout" or "declined". The text in `MESSAGES` is shown to customers,
and the support team rewrites it when they want to, including for translations.

`checkout.pay(charge, amount_cents)` takes a charge function and tries a timed-out
charge once more; the gateway guarantees a repeated charge after a timeout is applied
once.
