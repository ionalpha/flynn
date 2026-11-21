"""Push delivery. Every push notification goes through send_push."""


def send_push(user, msg):
    return "mobile %s %s" % (user["name"], msg["text"])
