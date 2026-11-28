class Store:
    def __init__(self, backend):
        self.backend = backend

    def get(self, key):
        return self.backend.read(key)

    def put(self, key, value):
        self.backend.write(key, value)
