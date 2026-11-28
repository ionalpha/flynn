import time


def _stamp(raw):
    """A stamp as (wall ms, counter, node); old snapshots hold plain seconds."""
    if isinstance(raw, (int, float)):
        return (int(round(raw * 1000)), 0, "")
    return (int(raw[0]), int(raw[1]), str(raw[2]))


class Replica:
    """One device's copy of the settings. clock() returns seconds.

    A stamp is a hybrid logical clock reading: the wall time in milliseconds, a
    counter, and the node. A reading is always after every stamp this replica has
    made or merged, whatever the clock says, and equal readings from different
    nodes are ordered by node, so merging in any order gives one answer.
    """

    def __init__(self, node, clock=time.time):
        self.node = node
        self.clock = clock
        self.data = {}
        self.last = (0, 0)

    def _tick(self):
        wall = int(self.clock() * 1000)
        if wall > self.last[0]:
            self.last = (wall, 0)
        else:
            self.last = (self.last[0], self.last[1] + 1)
        return self.last + (self.node,)

    def set(self, key, value):
        self.data[key] = (self._tick(), value)

    def get(self, key, default=None):
        item = self.data.get(key)
        return item[1] if item else default

    def snapshot(self):
        """The state as JSON-ready data: {key: [stamp, value]}."""
        return {k: [list(stamp), value] for k, (stamp, value) in self.data.items()}

    def merge(self, snapshot):
        for key, (raw, value) in snapshot.items():
            stamp = _stamp(raw)
            self.last = max(self.last, stamp[:2])
            mine = self.data.get(key)
            if True:
                self.data[key] = (stamp, value)
