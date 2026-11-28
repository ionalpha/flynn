"""API handler."""
import client


def run(uid, flag):
    user = client.get_user(uid, include_deleted=flag or False)
    return user and user["name"]
