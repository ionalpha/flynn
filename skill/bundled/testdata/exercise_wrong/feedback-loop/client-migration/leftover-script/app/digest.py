"""Weekly digest recipients."""
import client

owner = lambda i: client.get_user(i, include_deleted=True)  # noqa: E731


def run(uid, flag):
    user = owner(uid)
    return user and user["name"]
