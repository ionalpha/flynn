"""The usage page shown to customers."""
import quota


def render(used, limit):
    pct = quota.percent(used, limit, nearest=True)
    left = quota.remaining(used, limit)
    return "%d of %d units used (%d%%), %d left" % (used, limit, pct, left)
