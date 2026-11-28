"""Usage measures. Each takes (used, limit) in units."""


def percent(used, limit, nearest=False):
    """Whole percent of the limit used, counted down unless nearest is set.

    Plan thresholds rely on counting down: see docs/PLANS.md. Display code passes
    nearest=True.
    """
    if limit <= 0:
        return 0
    if nearest:
        return (used * 200 + limit) // (2 * limit)
    return used * 100 // limit


def remaining(used, limit):
    """Units left before the limit, never below zero."""
    return max(limit - used, 0)
