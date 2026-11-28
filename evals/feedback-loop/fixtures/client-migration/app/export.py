"""Nightly export owner."""
import functools

import client

OWNER = functools.partial(client.fetch_user, deleted_ok=True)


def run(uid, flag):
    user = OWNER(uid)
    return user and user["name"]
