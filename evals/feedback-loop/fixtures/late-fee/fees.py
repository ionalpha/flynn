"""Late fees for returned books."""
from datetime import date

PER_DAY = 25
CAP = 500


def late_fee(due, returned):
    """The fee in cents for a book due on due and returned on returned."""
    if returned < due:
        return 0
    days = (returned - due).days + 1
    return min(days * PER_DAY, CAP)
