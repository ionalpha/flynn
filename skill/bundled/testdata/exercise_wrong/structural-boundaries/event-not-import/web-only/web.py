import app  # noqa: F401
from adapters import mail
from domain import orders


def post_order(form):
    orders.place_order({"id": form["id"], "email": form["email"], "cents": int(form["cents"])})
    mail.send(form["email"], "Order confirmed", "We have your order %s." % form["id"])
