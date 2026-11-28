# notify

Turns events into messages and delivers each one on every channel the user has chosen.

Push notifications are sent by `send_push` in `push.py`. To change how push behaves, change that function.

- `python3 app.py users.json events.jsonl` prints one line per delivery: `<channel> <user> <text>`.
- `python3 digest.py users.json events.jsonl` prints one summary per user.
