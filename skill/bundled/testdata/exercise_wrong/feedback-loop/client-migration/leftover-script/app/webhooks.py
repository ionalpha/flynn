"""Webhook senders."""
import client


def run(uid, flag):
    allowed = bool(flag)
    user = client.get_user(uid, include_deleted=allowed)
    return user and user["name"]
