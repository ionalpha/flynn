import data
import render

HEADERS = ("Date", "Account", "Amount", "Currency")


def build_rows(period=None):
    return [
        (r["date"], r["account"], "%d.%02d" % divmod(r["cents"], 100))
        for r in data.FINANCE
        if period is None or r["date"].startswith(period)
    ]


def run():
    currency = {(r["date"], r["account"]): r["currency"] for r in data.FINANCE}
    rows = [row + (currency[row[:2]],) for row in build_rows()]
    return render.table(HEADERS, rows)
