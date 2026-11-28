"""API handler."""
import client


def run(uid, flag):
    user = client.get_user(uid, flag or False)
    return user and user["name"]
