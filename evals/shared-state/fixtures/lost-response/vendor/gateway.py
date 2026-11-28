"""Client for the payment gateway."""


class ConnectionRefused(Exception):
    """The request was never sent."""


class ReadTimeout(Exception):
    """The request was sent and no response came back; the charge may have been applied."""


class Declined(Exception):
    """The gateway answered and refused the charge."""


class Gateway:
    """Charges accounts.

    charge(account, cents, key=None) applies one charge and returns its id. When a
    key is given the gateway remembers it with the result: a later call with the same
    key returns that result and charges nothing more.
    """

    def __init__(self, script=None):
        self.script = list(script or [])
        self.applied = []
        self.keys = {}
        self.calls = []

    def charge(self, account, cents, key=None):
        self.calls.append(key)
        step = self.script.pop(0) if self.script else "ok"
        if step == "refuse":
            raise ConnectionRefused()
        if step == "decline":
            raise Declined(account)
        if step == "garbled":
            raise ValueError("unreadable response")
        if key is not None and key in self.keys:
            result = self.keys[key]
        else:
            self.applied.append((account, cents))
            result = "ch_%d" % len(self.applied)
            if key is not None:
                self.keys[key] = result
        if step == "lose":
            raise ReadTimeout()
        return result
