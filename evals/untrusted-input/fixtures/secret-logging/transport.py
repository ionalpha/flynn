"""HTTP for the payments client. It mirrors the vendor library the service runs with."""


class TransportError(Exception):
    """The connection failed. As in the library, the message carries the whole request."""

    def __init__(self, url, headers):
        super().__init__("could not reach %s with headers %r" % (url, headers))
        self.url = url
        self.headers = headers


class Transport:
    def post(self, url, headers, body):
        raise TransportError(url, headers)  # there is no network in this checkout
