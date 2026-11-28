"""Usage measures. Each takes (used, limit) in units."""


def percent(used, limit):
    """Whole percent of the limit used, to the nearest percent."""
    if limit <= 0:
        return 0
    return (used * 200 + limit) // (2 * limit)


def remaining(used, limit):
    """Units left before the limit, never below zero."""
    return max(limit - used, 0)
