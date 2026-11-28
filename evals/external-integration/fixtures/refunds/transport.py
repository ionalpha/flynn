"""HTTP for the service clients. Clients call transport.post so tests can replace it;
there is no network in this checkout."""

from dataclasses import dataclass, field


@dataclass
class Response:
    status: int
    headers: dict = field(default_factory=dict)
    body: str = ""


def post(url, body, headers=None):
    raise RuntimeError("no network here: replace transport.post in tests")
