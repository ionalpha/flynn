"""Mail recipients."""
import client


def run(uid, flag):
    user = getattr(client, "fetch_user")(uid, flag)
    return user and user["name"]
