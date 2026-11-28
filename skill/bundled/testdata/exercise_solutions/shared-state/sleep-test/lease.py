"""A lease a worker holds on a job. It expires ttl seconds after it was taken or renewed."""

import time


class Lease:
    def __init__(self, ttl, clock=time.monotonic):
        self.ttl = ttl
        self.clock = clock
        self.taken = clock()

    def expired(self):
        return self.clock() - self.taken >= self.ttl

    def renew(self):
        self.taken = self.clock()
