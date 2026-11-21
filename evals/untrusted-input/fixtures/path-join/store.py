import os

UPLOADS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "uploads")


def read_attachment(name):
    """Bytes of the attachment called name, which arrives in the request URL."""
    with open(os.path.join(UPLOADS, name), "rb") as f:
        return f.read()
