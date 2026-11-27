from dataclasses import dataclass

USERS = {"ana": "example-password"}


@dataclass
class Response:
    status: int
    location: str = ""


def login(params, form):
    """params is the url-decoded query string of the page, form the posted fields."""
    user = form.get("user")
    if user not in USERS or USERS[user] != form.get("password"):
        return Response(401)
    return Response(302, "/dashboard")
