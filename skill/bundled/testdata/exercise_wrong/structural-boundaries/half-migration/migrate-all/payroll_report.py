import data
import render


def run():
    rows = [(r["date"], r["name"], "%d.%02d" % divmod(r["cents"], 100)) for r in data.PAYROLL]
    return render.table(("Date", "Name", "Amount"), rows)
