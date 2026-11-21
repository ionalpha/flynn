"""Handling messages from the credits topic."""

_seen = set()


def handle(ledger, msg):
    if msg["id"] in _seen:
        return
    _seen.add(msg["id"])
    ledger.credit(msg["account"], msg["cents"])
