"""Note authors, cached first."""
import client
from cache import Cache

cache = Cache()


def run(uid, flag):
    cached = cache.fetch_user(uid)
    user = client.get_user(uid, flag)
    return [cached["name"], user and user["name"]]
