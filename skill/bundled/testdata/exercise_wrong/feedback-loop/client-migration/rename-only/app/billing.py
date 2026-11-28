"""Billing contact lookup. Closed accounts still get a final invoice."""
import client


def run(uid, flag):
    user = client.get_user(uid, True)
    return user and user["name"]
