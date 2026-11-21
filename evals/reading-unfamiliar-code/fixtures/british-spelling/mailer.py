import os

_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "emails")


def subject(status, order):
    with open(os.path.join(_DIR, status + ".txt"), encoding="utf-8") as f:
        return f.readline().rstrip("\n").format(id=order["id"])
