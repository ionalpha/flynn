"""Post a message to a team chat channel."""

import os

import transport

URL = "https://chat.example.test/v1/messages"


class AlertError(Exception):
    pass


def send_alert(channel, text):
    headers = {
        "Authorization": "Bearer " + os.environ.get("CHAT_TOKEN", ""),
        "Content-Type": "application/json",
    }
    resp = transport.post(URL, {"channel": channel, "text": text}, headers)
    if resp.status != 200:
        raise AlertError("chat answered %s" % resp.status)
    return True
