import data
import legacy_render


def run():
    rows = [("payroll", r["date"], r["name"], "%d.%02d" % divmod(r["cents"], 100)) for r in data.PAYROLL]
    return legacy_render.table(("Source", "Date", "Account", "Amount"), rows, (8, 10, 10, 12))
