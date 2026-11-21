"""Weekly digest recipients."""
import client

owner = lambda i: client.fetch_user(i, True)  # noqa: E731


def run(uid, flag):
    user = owner(uid)
    return user and user["name"]
