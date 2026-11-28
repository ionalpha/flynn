"""Team members."""
import client


def run(uid, flag):
    members = [client.get_user(i, include_deleted=flag) for i in (uid, uid + 1)]
    return [m and m["name"] for m in members]
