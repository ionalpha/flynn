"""Events from the payment provider's feed."""

from dataclasses import dataclass


class EventError(ValueError):
    pass


@dataclass
class Event:
    id: str
    status: str
    amount_cents: int


def parse_event(data):
    return Event(data.get("id"), data.get("status"), data.get("amount_cents"))
