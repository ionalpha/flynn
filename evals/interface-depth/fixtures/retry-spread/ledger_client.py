"""Client for the ledger service."""


class TransientError(Exception):
    """The request did not get through."""


class PermanentError(Exception):
    """The ledger rejected the entry."""


ENTRIES = []


def _local_transport(entry):
    ENTRIES.append(entry)
    return "rcpt-%d" % len(ENTRIES)


TRANSPORT = _local_transport


def post(entry):
    return TRANSPORT(entry)
