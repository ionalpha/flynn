import time


class Replica:
    """One device's copy of the settings. clock() returns seconds."""

    def __init__(self, node, clock=time.time):
        self.node = node
        self.clock = clock
        self.data = {}

    def set(self, key, value):
        self.data[key] = (self.clock(), value)

    def get(self, key, default=None):
        item = self.data.get(key)
        return item[1] if item else default

    def snapshot(self):
        """The state as JSON-ready data: {key: [stamp, value]}."""
        return {k: [stamp, value] for k, (stamp, value) in self.data.items()}

    def merge(self, snapshot):
        for key, (stamp, value) in snapshot.items():
            mine = self.data.get(key)
            if mine is None or stamp > mine[0]:
                self.data[key] = (stamp, value)
