import data
import legacy_render


def run():
    rows = [(r["date"], r["name"], "%d.%02d" % divmod(r["cents"], 100)) for r in data.PAYROLL]
    return legacy_render.table(("Date", "Name", "Amount"), rows, (10, 10, 12))
