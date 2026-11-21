"""Audit trail rows."""
import client


def run(uid, flag):
    user = client.get_user(
        uid,
        include_deleted=flag,  # audit views see deleted users
    )
    return user and user["name"]
