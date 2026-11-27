class MemoryBackend:
    """Stands in for the network backend, which raises OSError subclasses when unreachable."""

    def __init__(self):
        self.data = {}

    def read(self, key):
        return self.data[key]

    def write(self, key, value):
        self.data[key] = value
