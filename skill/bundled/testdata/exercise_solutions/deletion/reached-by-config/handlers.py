def on_signup(payload):
    return "welcome %s" % payload["name"]


def on_refund(payload):
    return "refunded %d cents to %s" % (payload["cents"], payload["name"])


def on_cancel(payload):
    return "cancelled %s" % payload["name"]
