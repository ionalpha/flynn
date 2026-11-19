"""Stand-in for the mail server. OUTBOX is what was delivered, CONNECTIONS what was opened."""

OUTBOX = []
CONNECTIONS = []


class Connection:
    def __init__(self, host):
        self.host = host

    def deliver(self, to, subject, body):
        OUTBOX.append((self.host, to, subject, body))


def open_connection(host):
    CONNECTIONS.append(host)
    return Connection(host)
