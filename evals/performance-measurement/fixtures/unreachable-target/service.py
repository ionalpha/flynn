from vendor.lookup import fetch


def handle(keys):
    return [fetch(k) for k in keys]
