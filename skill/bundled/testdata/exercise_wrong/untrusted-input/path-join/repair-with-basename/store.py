import os

UPLOADS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "uploads")


def read_attachment(name):
    with open(os.path.join(UPLOADS, os.path.basename(name)), "rb") as f:
        return f.read()
