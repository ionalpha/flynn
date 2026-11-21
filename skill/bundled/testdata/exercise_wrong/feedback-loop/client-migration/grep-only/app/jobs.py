"""Job owners."""
from client import get_user


def run(uid, flag):
    if flag:
        user = get_user(uid, include_deleted=True)
    else:
        user = get_user(uid)
    return user and user["name"]
