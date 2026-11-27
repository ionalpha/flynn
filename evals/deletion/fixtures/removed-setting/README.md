# relay

Forwards messages to a mail relay. `config.load(path)` reads an operator's file over
the defaults. An unknown key is an error, so that a misspelt setting cannot pass for
the default.

Operators write their own config files, keep them on their own machines and upgrade
relay in place. `examples/` holds the two files we ship as a starting point.

Redelivering a failed message is the queue's job.
