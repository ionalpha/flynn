"""Webhook senders."""
import client


def run(uid, flag):
    allowed = bool(flag)
    user = client.fetch_user(uid, deleted_ok=allowed)
    return user and user["name"]
