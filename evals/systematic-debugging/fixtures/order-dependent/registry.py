"""Feature flag registry, loaded once per process."""

_FLAGS = {"beta_checkout": False, "new_pricing": False}


def enable(name):
    _FLAGS[name] = True


def flags():
    return _FLAGS


def is_enabled(name):
    return _FLAGS.get(name, False)
