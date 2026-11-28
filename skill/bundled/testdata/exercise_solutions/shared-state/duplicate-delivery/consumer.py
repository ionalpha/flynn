"""Handling messages from the credits topic. The broker may deliver a message more than once."""


def handle(ledger, msg):
    ledger.credit(msg["account"], msg["cents"], msg["id"])
