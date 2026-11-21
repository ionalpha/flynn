"""Handling messages from the credits topic."""


def handle(ledger, msg):
    ledger.credit(msg["account"], msg["cents"])
