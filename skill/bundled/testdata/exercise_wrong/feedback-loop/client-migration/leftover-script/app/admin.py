"""Admin console. fetch_user(uid, True) is what the console used before 2.0."""
import client


def run(uid, flag):
    if flag:
        user = client.get_user(uid, include_deleted=True)
    else:
        user = client.get_user(uid)
    return user and user["name"]
