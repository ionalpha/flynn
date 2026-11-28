# payment webhook

The provider delivers each event at least once and keeps sending it until `webhook.handle` returns True. A handler that raises or returns False gets the same event again later. The ledger is a remote service and is sometimes unavailable.
