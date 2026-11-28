class PageCache:
    """Keeps rendered pages until the thing they show changes."""

    def __init__(self):
        self._pages = {}

    def wrap(self, render):
        def cached(*args):
            variants = self._pages.setdefault(args[0], {})
            if args[1:] not in variants:
                variants[args[1:]] = render(*args)
            return variants[args[1:]]

        return cached

    def drop(self, key):
        self._pages.pop(key, None)
