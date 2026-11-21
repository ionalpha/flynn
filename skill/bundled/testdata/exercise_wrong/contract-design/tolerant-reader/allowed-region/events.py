"""Events from the payment provider's feed."""

from dataclasses import dataclass

FIELDS = {"id", "status", "amount_cents", "region"}
STATUSES = {"paid", "refunded", "failed"}


class EventError(ValueError):
    pass


@dataclass
class Event:
    id: str
    status: str
    amount_cents: int


def parse_event(data):
    unknown = set(data) - FIELDS
    if unknown:
        raise EventError("unknown fields: " + ", ".join(sorted(unknown)))
    if data.get("status") not in STATUSES:
        raise EventError("unknown status: " + repr(data.get("status")))
    amount = data.get("amount_cents")
    if type(amount) is not int:
        raise EventError("amount_cents must be an integer")
    return Event(data["id"], data["status"], amount)
