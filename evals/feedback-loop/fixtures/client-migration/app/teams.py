"""Team members."""
import client


def run(uid, flag):
    members = [client.fetch_user(i, deleted_ok=flag) for i in (uid, uid + 1)]
    return [m and m["name"] for m in members]
