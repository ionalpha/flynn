"""Admin console. fetch_user(uid, True) is what the console used before 2.0."""
import client


def run(uid, flag):
    if flag:
        user = client.fetch_user(uid, True)
    else:
        user = client.fetch_user(uid)
    return user and user["name"]
