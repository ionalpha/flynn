class PageCache:
    """Keeps rendered pages until the thing they show changes."""

    def __init__(self):
        self._pages = {}

    def wrap(self, render):
        def cached(*args):
            key = args[0]
            if key not in self._pages:
                self._pages[key] = render(*args)
            return self._pages[key]

        return cached

    def drop(self, key):
        self._pages.pop(key, None)
