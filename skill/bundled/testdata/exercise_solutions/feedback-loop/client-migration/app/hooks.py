"""Webhook subjects."""
import client

READERS = {"live": client.get_user, "archive": client.get_user}


def run(uid, flag):
    user = READERS["archive"](uid, include_deleted=True) if flag else READERS["live"](uid)
    return user and user["name"]
