"""Invoice owner."""
import client


def run(uid, flag):
    user = client.fetch_user(uid, deleted_ok=flag)
    return user and user["name"]
