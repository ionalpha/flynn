"""Audit trail rows."""
import client


def run(uid, flag):
    user = client.fetch_user(
        uid,
        deleted_ok=flag,  # audit views see deleted users
    )
    return user and user["name"]
