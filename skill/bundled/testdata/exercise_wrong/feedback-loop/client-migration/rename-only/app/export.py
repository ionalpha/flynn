"""Nightly export owner."""
import functools

import client

OWNER = functools.partial(client.get_user, include_deleted=True)


def run(uid, flag):
    user = OWNER(uid)
    return user and user["name"]
