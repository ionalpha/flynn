"""cronlet: the next time a five-field cron expression fires."""

from datetime import datetime, timedelta

_RANGES = ((0, 59), (0, 23), (1, 31), (1, 12), (0, 7))


def _expand_field(text, lo, hi):
    out = set()
    for part in text.split(","):
        step = 1
        if "/" in part:
            part, s = part.split("/", 1)
            step = int(s)
            if step < 1:
                raise ValueError("bad step in %r" % text)
        if part == "*":
            a, b = lo, hi
        elif "-" in part:
            a, b = (int(x) for x in part.split("-", 1))
        else:
            a = int(part)
            b = hi if "/" in text else a
        if a < lo or b > hi or a > b:
            raise ValueError("%r is out of range" % text)
        out.update(range(a, b + 1, step))
    return out


def next_after(expr, after):
    fields = expr.split()
    if len(fields) != 5:
        raise ValueError("a cron expression has five fields")
    mins, hours, doms, months, dows = (_expand_field(f, lo, hi) for f, (lo, hi) in zip(fields, _RANGES))
    dows = {d % 7 for d in dows}
    either = not (fields[2].startswith("*") or fields[4].startswith("*"))
    t = after.replace(second=0, microsecond=0) + timedelta(minutes=1)
    while t.year <= after.year + 9:
        if t.month not in months:
            t = datetime(t.year + (t.month == 12), t.month % 12 + 1, 1, tzinfo=t.tzinfo)
            continue
        wd = (t.weekday() + 1) % 7
        day_ok = (t.day in doms or wd in dows) if either else (t.day in doms and wd in dows)
        if not day_ok:
            t = (t + timedelta(days=1)).replace(hour=0, minute=0)
            continue
        if t.hour not in hours:
            t = t.replace(minute=0) + timedelta(hours=1)
            continue
        if t.minute not in mins:
            t += timedelta(minutes=1)
            continue
        return t
    raise ValueError("%r never fires" % expr)

__version__ = "1.4.0"
