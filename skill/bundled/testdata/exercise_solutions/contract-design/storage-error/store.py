class StorageError(OSError):
    """The backend could not be reached. An OSError, so callers that retry still do."""


class Store:
    def __init__(self, backend):
        self.backend = backend

    def get(self, key):
        try:
            return self.backend.read(key)
        except OSError as e:
            raise StorageError("read of %r failed: %s" % (key, e)) from e

    def put(self, key, value):
        try:
            self.backend.write(key, value)
        except OSError as e:
            raise StorageError("write of %r failed: %s" % (key, e)) from e
