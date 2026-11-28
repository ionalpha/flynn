"""Mail recipients."""
import client


def run(uid, flag):
    user = getattr(client, "get_user")(uid, include_deleted=flag)
    return user and user["name"]
