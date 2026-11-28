"""Invoice owner."""
import client


def run(uid, flag):
    user = client.get_user(uid, include_deleted=flag)
    return user and user["name"]
