import time

STORE = {}


def now():
    return time.time()


def put(key, value, ttl):
    STORE[key] = (value, now() + ttl)


def get(key):
    entry = STORE.get(key)
    if entry is None or entry[1] <= now():
        return None
    return entry[0]
