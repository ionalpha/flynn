"""Test double for the card gateway, kept in step with the platform team's client. Do not edit.

A call can fail two ways that look the same to the caller: the request never arrived
("lost"), or it was applied and the reply never came back ("late").
"""


class Timeout(Exception):
    """The caller does not know whether the call was applied."""


class Gateway:
    def __init__(self, script=()):
        """script[i] is what happens to call number i: "ok", "lost" or "late". Calls past the end succeed."""
        self.script = list(script)
        self.calls = 0
        self.held = {}
        self.answered = 0

    def _step(self):
        mode = self.script[self.calls] if self.calls < len(self.script) else "ok"
        self.calls += 1
        return mode

    def charge(self, order_id, cents, key=None):
        """Charge once per key. Without a key every call is a new charge."""
        mode = self._step()
        if mode == "lost":
            raise Timeout()
        ref = key if key is not None else "anon-%d" % len(self.held)
        self.held.setdefault(ref, (order_id, cents))
        if mode == "late":
            raise Timeout()
        return ref

    def status(self, key):
        """Whether the gateway holds a charge under key."""
        if self._step() != "ok":
            raise Timeout()
        self.answered += 1
        return key in self.held

    def charges_for(self, order_id):
        return sum(1 for owner, _ in self.held.values() if owner == order_id)
