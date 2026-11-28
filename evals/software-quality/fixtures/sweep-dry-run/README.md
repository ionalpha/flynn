# Upload sweep

Cron runs this every night over the shared upload directory:

    python sweep.py DIR --today YYYY-MM-DD

`DIR/catalog.json` lists each upload with its owner and expiry date. An expired
upload gets a day's notice: the first run that finds it expired marks it and writes
a line to `DIR/outbox.log`, which the mailer sends to the owner, and a run on a
later day removes it.

Run the tests with `python -m unittest`.
