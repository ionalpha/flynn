"""Reading and renaming user profiles."""


def get_profile(db, cache, user_id):
    """The profile, from the cache when it is there."""
    key = "profile:%s" % user_id
    found = cache.get(key)
    if found is not None:
        return found
    row = db.read(user_id)
    cache.set(key, row)
    return row


def update_name(db, cache, user_id, name):
    cache.delete("profile:%s" % user_id)
    db.write(user_id, name)
