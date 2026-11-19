# credits consumer

The broker client calls consumer.handle for each message on the credits topic. Each
message has a unique id, an account and an amount in cents. The broker delivers at
least once: after the consumer restarts it redelivers whatever it had not seen
acknowledged. ledger.py keeps balances in a JSON file so they survive a restart.
