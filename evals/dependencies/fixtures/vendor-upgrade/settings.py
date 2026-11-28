from vendor.envfile import parse


def load(path):
    with open(path, encoding="utf-8") as f:
        return parse(f.read())
