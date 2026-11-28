def get_or_default(store, key, default):
    try:
        return store.get(key)
    except KeyError:
        return default
