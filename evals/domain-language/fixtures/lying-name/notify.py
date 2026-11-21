"""Customer email. SENT records what went out, for the outbox page."""

SENT = []


def send_reminder(invoice):
    SENT.append(invoice)
