"""Job owners."""
from client import fetch_user


def run(uid, flag):
    if flag:
        user = fetch_user(uid, True)
    else:
        user = fetch_user(uid)
    return user and user["name"]
