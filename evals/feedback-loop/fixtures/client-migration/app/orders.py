"""Order owner."""
import client


def run(uid, flag):
    user = client.fetch_user(user_id=uid, deleted_ok=flag)
    return user and user["name"]
