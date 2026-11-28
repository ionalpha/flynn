"""Directory sync."""
import client


def load(uid, flag, fetch=client.get_user):
    return fetch(uid, flag)


def run(uid, flag):
    user = load(uid, flag)
    return user and user["name"]
