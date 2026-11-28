"""Session owner."""
import client as directory


def run(uid, flag):
    user = directory.get_user(uid, include_deleted=flag)
    return user and user["name"]
