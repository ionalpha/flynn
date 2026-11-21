"""Customer email."""

import smtp

HOST = "smtp.example.com"


class Mailer:
    def __init__(self, host):
        self.host = host
        self._conn = None

    def connect(self):
        """Open the connection now instead of at the first send. Callers never need to."""
        if self._conn is None:
            self._conn = smtp.open_connection(self.host)

    def send(self, to, subject, body):
        self.connect()
        self._conn.deliver(to, subject, body)
