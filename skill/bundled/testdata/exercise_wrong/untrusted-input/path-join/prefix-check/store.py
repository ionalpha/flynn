import os

UPLOADS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "uploads")


def read_attachment(name):
    path = os.path.normpath(os.path.join(UPLOADS, name))
    if not path.startswith(UPLOADS):
        raise ValueError("bad attachment name")
    with open(path, "rb") as f:
        return f.read()
