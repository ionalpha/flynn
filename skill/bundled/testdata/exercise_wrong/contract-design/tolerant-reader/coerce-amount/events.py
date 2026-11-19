"""Events from the payment provider's feed."""

from dataclasses import dataclass

FIELDS = {"id", "status", "amount_cents"}
STATUSES = {"paid", "refunded", "failed"}


class EventError(ValueError):
    pass


@dataclass
class Event:
    id: str
    status: str
    amount_cents: int


def parse_event(data):
    try:
        amount = int(data["amount_cents"])
    except (KeyError, TypeError, ValueError):
        raise EventError("amount_cents must be an integer")
    return Event(data["id"], data.get("status", "unknown"), amount)
