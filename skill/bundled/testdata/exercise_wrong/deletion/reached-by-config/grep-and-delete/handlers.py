def on_signup(payload):
    return "welcome %s" % payload["name"]
