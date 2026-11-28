class Secret:
    """A credential that renders as a marker everywhere except through reveal()."""

    __slots__ = ("_value",)

    def __init__(self, value):
        self._value = value

    def reveal(self):
        return self._value

    def __repr__(self):
        return "Secret([REDACTED])"

    __str__ = __repr__

    def __format__(self, spec):
        return "[REDACTED]"
