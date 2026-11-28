"""Post a message to a team chat channel."""

import json
import logging
import os
import time

import transport

URL = "https://chat.example.test/v1/messages"
MAX_ATTEMPTS = 3
MAX_WAIT = 30
MAX_TEXT = 3000

log = logging.getLogger("alerts")


class AlertError(Exception):
    pass


def _fit(text):
    if len(text) <= MAX_TEXT:
        return text
    return text[: MAX_TEXT - 3] + "..."


def send_alert(channel, text):
    """Best effort: True when posted, False after the reason has been logged.

    The caller is handling a failed job; a chat problem must not replace that failure
    or stop the jobs after it.
    """
    text = _fit(text)
    headers = {
        "Authorization": "Bearer " + os.environ.get("CHAT_TOKEN", ""),
        "Content-Type": "application/json",
    }
    reason = "no attempt made"
    for attempt in range(MAX_ATTEMPTS):
        wait = 0.5 * 2**attempt
        try:
            resp = transport.post(URL, {"channel": channel, "text": text}, headers)
        except TimeoutError:
            reason = "timed out"
        else:
            if resp.status in (502, 503, 504):
                reason = "chat answered %s" % resp.status
            elif resp.status != 200:
                reason = "chat answered %s" % resp.status
                break
            else:
                try:
                    data = json.loads(resp.body)
                except ValueError:
                    reason = "chat answered 200 with a body that is not JSON"
                    break
                if data.get("ok"):
                    return True
                reason = data.get("error", "unknown error")
                if reason != "ratelimited":
                    break
                wait = data.get("retry_after", 1)
                if wait > MAX_WAIT:
                    break
        if attempt < MAX_ATTEMPTS - 1:
            time.sleep(wait)
    log.warning("alert to %s not delivered: %s", channel, reason)
    return False
