import data
import legacy_render

HEADERS = ("Date", "Account", "Amount", "Currency")
WIDTHS = (10, 10, 12, 8)


def build_rows(period=None):
    return [
        (r["date"], r["account"], "%d.%02d" % divmod(r["cents"], 100))
        for r in data.FINANCE
        if period is None or r["date"].startswith(period)
    ]


def run():
    currency = {(r["date"], r["account"]): r["currency"] for r in data.FINANCE}
    rows = [row + (currency[row[:2]],) for row in build_rows()]
    return legacy_render.table(HEADERS, rows, WIDTHS)
