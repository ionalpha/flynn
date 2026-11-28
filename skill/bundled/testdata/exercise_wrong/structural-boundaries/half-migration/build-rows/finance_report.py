import data
import legacy_render

HEADERS = ("Date", "Account", "Amount", "Currency")
WIDTHS = (10, 10, 12, 8)


def build_rows(period=None):
    return [
        (r["date"], r["account"], "%d.%02d" % divmod(r["cents"], 100), r["currency"])
        for r in data.FINANCE
        if period is None or r["date"].startswith(period)
    ]


def run():
    return legacy_render.table(HEADERS, build_rows(), WIDTHS)
