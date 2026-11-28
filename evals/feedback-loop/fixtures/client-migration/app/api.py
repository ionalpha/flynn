"""API handler."""
import client


def run(uid, flag):
    user = client.fetch_user(uid, flag or False)
    return user and user["name"]
