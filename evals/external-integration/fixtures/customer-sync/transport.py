"""HTTP for the service clients. Clients call transport.get so tests can replace it;
there is no network in this checkout."""

from dataclasses import dataclass, field


@dataclass
class Response:
    status: int
    headers: dict = field(default_factory=dict)
    body: str = ""


def get(url, headers=None):
    raise RuntimeError("no network here: replace transport.get in tests")
