"""Session owner."""
import client as directory


def run(uid, flag):
    user = directory.get_user(uid, flag)
    return user and user["name"]
