_subscribers = {}


def subscribe(name, fn):
    _subscribers.setdefault(name, []).append(fn)


def publish(name, payload):
    for fn in _subscribers.get(name, []):
        fn(payload)
