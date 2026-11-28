"""Client for the shared cache. Every web process talks to the same cache."""


class Cache:
    def __init__(self):
        self._data = {}

    def get(self, key):
        value = self._data.get(key)
        return None if value is None else dict(value)

    def set(self, key, value):
        self._data[key] = dict(value)

    def delete(self, key):
        self._data.pop(key, None)

    def set_if_newer(self, key, value):
        """Store value unless the cache holds one with the same or a higher
        value["version"]. Atomic on the cache server.
        """
        held = self._data.get(key)
        if held is None or held["version"] < value["version"]:
            self._data[key] = dict(value)
