"""Monthly report owner."""
from client import get_user as lookup


def run(uid, flag):
    user = lookup(uid, include_deleted=flag)
    return user and user["name"]
