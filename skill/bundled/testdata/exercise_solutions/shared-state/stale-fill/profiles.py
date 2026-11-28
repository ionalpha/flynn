"""Reading and renaming user profiles."""


def get_profile(db, cache, user_id):
    """The profile, from the cache when it is there."""
    key = "profile:%s" % user_id
    found = cache.get(key)
    if found is not None:
        return found
    row = db.read(user_id)
    cache.set_if_newer(key, row)
    return row


def update_name(db, cache, user_id, name):
    row = db.write(user_id, name)
    cache.set_if_newer("profile:%s" % user_id, row)
