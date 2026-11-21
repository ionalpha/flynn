"""Session owner."""
import client as directory


def run(uid, flag):
    user = directory.fetch_user(uid, flag)
    return user and user["name"]
