import data
import finance_report
import legacy_render


def run():
    rows = [("finance",) + r for r in finance_report.build_rows()]
    rows += [("payroll", r["date"], r["name"], "%d.%02d" % divmod(r["cents"], 100)) for r in data.PAYROLL]
    return legacy_render.table(("Source", "Date", "Account", "Amount"), rows, (8, 10, 10, 12))
