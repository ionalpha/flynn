"""Client for the profile service. Vendored."""

import threading
import time

MAX_IN_FLIGHT = 8


class NotFound(Exception):
    pass


class TooBusy(Exception):
    pass


class Client:
    """One connection to the service, safe to share between threads.

    The service allows MAX_IN_FLIGHT requests in flight per client and rejects
    one more with TooBusy.
    """

    def __init__(self, latency=0.05):
        self._latency = latency
        self._lock = threading.Lock()
        self._in_flight = 0

    def get(self, user_id):
        with self._lock:
            if self._in_flight >= MAX_IN_FLIGHT:
                raise TooBusy(user_id)
            self._in_flight += 1
        try:
            time.sleep(self._latency)
            if user_id < 0:
                raise NotFound(user_id)
            return {"id": user_id, "name": "user %d" % user_id}
        finally:
            with self._lock:
                self._in_flight -= 1
