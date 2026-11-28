class Seen:
    """Event ids, kept across deliveries."""

    def __init__(self):
        self._ids = set()

    def add(self, event_id):
        self._ids.add(event_id)

    def __contains__(self, event_id):
        return event_id in self._ids
