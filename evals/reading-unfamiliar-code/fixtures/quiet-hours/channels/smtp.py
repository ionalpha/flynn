def deliver(user, msg):
    return "email %s %s" % (user["name"], msg["text"])
