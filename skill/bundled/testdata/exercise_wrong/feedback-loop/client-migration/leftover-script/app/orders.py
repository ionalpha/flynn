"""Order owner."""
import client


def run(uid, flag):
    user = client.get_user(user_id=uid, include_deleted=flag)
    return user and user["name"]
