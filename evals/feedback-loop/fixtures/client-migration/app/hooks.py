"""Webhook subjects."""
import client

READERS = {"live": client.fetch_user, "archive": client.fetch_user}


def run(uid, flag):
    user = READERS["archive"](uid, True) if flag else READERS["live"](uid)
    return user and user["name"]
