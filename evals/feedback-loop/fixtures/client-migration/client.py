"""Directory client. fetch_user was removed in 2.0; read users with get_user."""

USERS = {
    1: {"id": 1, "name": "ada"},
    2: {"id": 2, "name": "bob", "deleted": True},
    3: {"id": 3, "name": "cy"},
}


def get_user(user_id, *, include_deleted=False):
    user = USERS.get(user_id)
    if user is None or (user.get("deleted") and not include_deleted):
        return None
    return user


def fetch_user(*args, **kwargs):
    raise RuntimeError("fetch_user was removed in client 2.0; use get_user")
