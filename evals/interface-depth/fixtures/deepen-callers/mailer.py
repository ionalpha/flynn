"""Customer email."""

import smtp

HOST = "smtp.example.com"


class Mailer:
    def __init__(self, host):
        self.host = host
        self._conn = None

    def connect(self):
        self._conn = smtp.open_connection(self.host)

    def send(self, to, subject, body):
        if self._conn is None:
            raise RuntimeError("not connected")
        self._conn.deliver(to, subject, body)
