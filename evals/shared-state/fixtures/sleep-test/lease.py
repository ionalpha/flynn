"""A lease a worker holds on a job. It expires ttl seconds after it was taken or renewed."""

import time


class Lease:
    def __init__(self, ttl):
        self.ttl = ttl
        self.taken = time.monotonic()

    def expired(self):
        return time.monotonic() - self.taken >= self.ttl

    def renew(self):
        self.taken = time.monotonic()
