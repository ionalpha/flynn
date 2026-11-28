# ledger

Every money movement is recorded in the ledger service through
`ledger_client.post(entry)`, which returns a receipt id.

The ledger raises `TransientError` when the network drops a request and
`PermanentError` when it rejects the entry itself. A rejected entry is wrong and
sending it again changes nothing; a dropped request is worth repeating a few times,
about 1 second apart.

`invoices`, `refunds`, `payouts`, `adjustments` and `signup_credit` post as they
happen. `nightly_export` is the job the scheduler runs each night.
