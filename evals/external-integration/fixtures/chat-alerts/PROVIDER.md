# Team chat: posting messages

Base URL `https://chat.example.test/v1`. Requests and responses are JSON.

## POST /messages

Body: `{"channel": "#ops", "text": "..."}`. `text` is at most 3000 characters.

The service answers 200 for every request its own servers handled, whether or not the
message was posted. The body says which:

```json
{"ok": true, "ts": "1700000000.0001"}
{"ok": false, "error": "ratelimited", "retry_after": 2}
{"ok": false, "error": "channel_not_found"}
{"ok": false, "error": "msg_too_long"}
{"ok": false, "error": "invalid_auth"}
```

- `ratelimited` clears after `retry_after` seconds. The service asks for at most 3
  attempts per message.
- Every other `error` is final: sending the same message again gets the same answer.

## Gateway failures

502, 503 and 504 come from the load balancer in front of the service and clear within
seconds. The same 3 attempts apply. Nothing was posted when one of them is returned.

## Who calls this

Alerts are sent from the nightly runner while it is handling a failed job. A chat
outage must not stop the rest of the night's jobs, and an alert that could not be
delivered has to leave a trace in the log so someone knows to look.
