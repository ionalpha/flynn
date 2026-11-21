"""Profile page data."""
import client


def run(uid, flag):
    user = client.get_user(uid)
    return user and user["name"]
