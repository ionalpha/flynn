import os

UPLOADS = os.path.realpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "uploads"))


def read_attachment(name):
    """Bytes of the attachment called name, which arrives in the request URL."""
    if not isinstance(name, str) or not name or "\x00" in name:
        raise ValueError("bad attachment name")
    path = os.path.realpath(os.path.join(UPLOADS, name))
    if os.path.commonpath([UPLOADS, path]) != UPLOADS:
        raise ValueError("bad attachment name")
    with open(path, "rb") as f:
        return f.read()
