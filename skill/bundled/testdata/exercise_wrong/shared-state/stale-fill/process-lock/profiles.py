"""Reading and renaming user profiles."""

import threading

_lock = threading.RLock()


def get_profile(db, cache, user_id):
    """The profile, from the cache when it is there."""
    key = "profile:%s" % user_id
    with _lock:
        found = cache.get(key)
        if found is not None:
            return found
        row = db.read(user_id)
        cache.set(key, row)
        return row


def update_name(db, cache, user_id, name):
    with _lock:
        db.write(user_id, name)
        cache.delete("profile:%s" % user_id)
