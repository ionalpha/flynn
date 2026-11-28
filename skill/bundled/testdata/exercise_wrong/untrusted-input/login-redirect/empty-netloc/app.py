from urllib.parse import urlparse
from dataclasses import dataclass

USERS = {"ana": "example-password"}


@dataclass
class Response:
    status: int
    location: str = ""


def _on_site(target):
    return target if target and not urlparse(target).netloc else "/dashboard"


def login(params, form):
    """params is the url-decoded query string of the page, form the posted fields."""
    user = form.get("user")
    if user not in USERS or USERS[user] != form.get("password"):
        return Response(401)
    return Response(302, _on_site(params.get("next", "")))
