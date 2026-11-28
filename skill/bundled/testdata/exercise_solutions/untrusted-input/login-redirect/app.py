from dataclasses import dataclass

USERS = {"ana": "example-password"}


@dataclass
class Response:
    status: int
    location: str = ""


def _on_site(target):
    # Only a path on this site: "//host" and "/\\host" are other hosts to a browser, which
    # also drops tabs and newlines before reading the value.
    if not target.startswith("/") or target.startswith("//") or "\\" in target:
        return "/dashboard"
    if any(ord(c) < 32 for c in target):
        return "/dashboard"
    return target


def login(params, form):
    """params is the url-decoded query string of the page, form the posted fields."""
    user = form.get("user")
    if user not in USERS or USERS[user] != form.get("password"):
        return Response(401)
    return Response(302, _on_site(params.get("next", "")))
