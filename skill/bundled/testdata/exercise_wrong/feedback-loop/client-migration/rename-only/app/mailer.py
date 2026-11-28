"""Mail recipients."""
import client


def run(uid, flag):
    user = getattr(client, "get_user")(uid, flag)
    return user and user["name"]
