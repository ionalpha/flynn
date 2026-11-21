"""Events from the payment provider's feed."""

from dataclasses import dataclass

STATUSES = {"paid", "refunded", "failed"}
UNKNOWN = "unknown"


class EventError(ValueError):
    pass


@dataclass
class Event:
    id: str
    status: str
    amount_cents: int
    raw_status: str = ""


def parse_event(data):
    """Read the fields this service uses and ignore the rest.

    The provider adds fields and statuses without notice. A status not in STATUSES
    becomes UNKNOWN, with the provider's value kept in raw_status.
    """
    amount = data.get("amount_cents")
    if type(amount) is not int:
        raise EventError("amount_cents must be an integer")
    raw = data.get("status")
    if not isinstance(raw, str):
        raise EventError("status must be a string")
    return Event(data["id"], raw if raw in STATUSES else UNKNOWN, amount, raw)
