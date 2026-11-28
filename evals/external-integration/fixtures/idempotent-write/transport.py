"""HTTP for the service clients. Clients call transport.get and transport.post so tests
can replace them; there is no network in this checkout."""

from dataclasses import dataclass, field


@dataclass
class Response:
    status: int
    headers: dict = field(default_factory=dict)
    body: str = ""


def get(url, headers=None):
    raise RuntimeError("no network here: replace transport.get in tests")


def post(url, body, headers=None):
    raise RuntimeError("no network here: replace transport.post in tests")
