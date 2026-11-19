"""Client for the stock database.

get returns (value, version). put writes a value; given expected_version, the
database refuses the write with Conflict if the row has moved since that version.
"""


class Conflict(Exception):
    pass


class Store:
    def __init__(self, data=None):
        self._rows = {k: (v, 1) for k, v in (data or {}).items()}

    def get(self, key):
        return self._rows[key]

    def put(self, key, value, expected_version=None):
        current = self._rows.get(key, (None, 0))[1]
        if expected_version is not None and current != expected_version:
            raise Conflict(key)
        self._rows[key] = (value, current + 1)
