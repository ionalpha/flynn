SENT = []


def send(to, subject, body):
    SENT.append((to, subject, body))
