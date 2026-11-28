import app  # noqa: F401
from domain import orders


def post_order(form):
    orders.place_order({"id": form["id"], "email": form["email"], "cents": int(form["cents"])})
