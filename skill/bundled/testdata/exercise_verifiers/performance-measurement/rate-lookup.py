import sys, time
sys.path.insert(0, ".")
fails = []
try:
    from fx import convert

    def r(c, d, v):
        return {"currency": c, "day": d, "rate": v}

    def t(c, d):
        return {"currency": c, "day": d, "amount": 10}

    feed = [r("eur", "2026-01-05", 5), r("eur", "2026-01-01", 1), r("gbp", "2026-01-02", 7),
            r("eur", "2026-01-03", 9), r("eur", "2026-01-03", 3), r("eur", "2026-01-02", 2)]
    asks = [t("eur", "2026-01-01"), t("eur", "2026-01-02"), t("eur", "2026-01-03"),
            t("eur", "2026-01-04"), t("eur", "2026-02-01"), t("gbp", "2026-01-09")]
    got = convert(asks, feed)
    if got != [10, 20, 30, 30, 50, 70]:
        fails.append("wrong amounts for a feed with late and corrected rates: %r" % got)
    got = convert(asks[:1], [r("eur", "2026-01-01", 4)])
    if got != [40]:
        fails.append("a second call with a different feed gave %r" % got)
    for missing in (t("eur", "2025-12-31"), t("chf", "2026-01-01")):
        try:
            convert([missing], feed)
            fails.append("no LookupError when there is no rate for %s on %s" % (missing["currency"], missing["day"]))
        except LookupError:
            pass

    days = ["d%05d" % i for i in range(20000)]
    big = [r("eur", d, i) for i, d in enumerate(days)]
    asks = [t("eur", days[(i * 7919) % 20000]) for i in range(20000)]
    for count, limit in ((500, 0.5), (20000, 4)):
        start = time.process_time()
        got = convert(asks[:count], big)
        spent = time.process_time() - start
        if got != [10 * ((i * 7919) % 20000) for i in range(count)]:
            fails.append("wrong amounts on the large feed")
        if spent > limit:
            fails.append("%d transactions against 20000 rates took %.1fs of CPU" % (count, spent))
            break
except Exception as e:
    fails.append("convert could not be exercised: %r" % e)
for x in fails:
    print(x, file=sys.stderr)
sys.exit(1 if fails else 0)
