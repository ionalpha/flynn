"""Note authors, cached first."""
import client
from cache import Cache

cache = Cache()


def run(uid, flag):
    cached = cache.get_user(uid)
    user = client.get_user(uid, include_deleted=flag)
    return [cached["name"], user and user["name"]]
