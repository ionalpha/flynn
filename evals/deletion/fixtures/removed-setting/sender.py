SENT = []


def send(settings, message):
    SENT.append((settings["host"], int(settings["port"]), float(settings["timeout_seconds"]), message))
