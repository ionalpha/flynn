"""Search result owners."""
from client import get_user


def run(uid, flag):
    return [u and u["name"] for u in map(get_user, [uid, uid + 1])]
