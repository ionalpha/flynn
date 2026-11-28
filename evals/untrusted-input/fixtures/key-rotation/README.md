# partner webhooks

`webhooks.verify(headers, body)` checks the signature on an event a partner posts to
us and returns the parsed event. The signature is an HMAC-SHA256 over the
`X-Timestamp` header, a dot and the raw body, sent in `X-Signature`.

Partners that have moved to key ids send `X-Key-Id` naming the key they signed with.
Partners that have not still sign with the old key and send no key id; that stays
accepted until they migrate. `keys.py` lists the keys. `RUNBOOK.md` is the on-call
page for this endpoint.

Run the tests with `python3 -m unittest discover -q`.
