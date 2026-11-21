"""Client for the ledger service."""
import time


class TransientError(Exception):
    """The request did not get through."""


class PermanentError(Exception):
    """The ledger rejected the entry."""


ENTRIES = []

ATTEMPTS = 3
PAUSE_SECONDS = 1


def _local_transport(entry):
    ENTRIES.append(entry)
    return "rcpt-%d" % len(ENTRIES)


TRANSPORT = _local_transport


def post(entry):
    """Send entry and return its receipt id.

    A dropped request is repeated, so TransientError means the ledger stayed
    unreachable through every attempt. PermanentError is never repeated.
    """
    for attempt in range(ATTEMPTS):
        try:
            return TRANSPORT(entry)
        except TransientError:
            if attempt == ATTEMPTS - 1:
                raise
            time.sleep(PAUSE_SECONDS)
