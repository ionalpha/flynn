"""Search result owners."""
from client import fetch_user


def run(uid, flag):
    return [u and u["name"] for u in map(fetch_user, [uid, uid + 1])]
