"""The usage page shown to customers."""
import quota


def render(used, limit):
    pct = quota.percent(used, limit)
    left = quota.remaining(used, limit)
    if limit > 0:
        pct = round(used * 100 / limit, 1)
    return "%d of %d units used (%s%%), %d left" % (used, limit, pct, left)
