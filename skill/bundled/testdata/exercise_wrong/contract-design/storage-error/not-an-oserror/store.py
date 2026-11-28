class StorageError(Exception):
    pass


class Store:
    def __init__(self, backend):
        self.backend = backend

    def get(self, key):
        try:
            return self.backend.read(key)
        except OSError as e:
            raise StorageError(str(e)) from e

    def put(self, key, value):
        try:
            self.backend.write(key, value)
        except OSError as e:
            raise StorageError(str(e)) from e
