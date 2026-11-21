import copy
import sys
from datetime import date

sys.path.insert(0, ".")
fails = []
TODAY = date(2026, 3, 10)


def book():
    return [
        {"id": "i1", "due": date(2026, 3, 1), "paid": False, "status": "open", "customer": "c1"},
        {"id": "i2", "due": date(2026, 3, 20), "paid": False, "status": "open", "customer": "c2"},
        {"id": "i3", "due": date(2026, 2, 1), "paid": True, "status": "paid", "customer": "c3"},
    ]


try:
    import dashboard
    import invoices
    import notify
    import reminders

    if hasattr(invoices, "is_overdue"):
        b = book()
        before = copy.deepcopy(b)
        answers = [invoices.is_overdue(i, TODAY) for i in b]
        if answers != [True, False, False]:
            fails.append("is_overdue answers %r, want [True, False, False]" % (answers,))
        if b != before or notify.SENT:
            fails.append("is_overdue changes the invoice or sends a reminder; a name starting is_ promises a question with no side effects")
    notify.SENT.clear()
    b = book()
    before = copy.deepcopy(b)
    dashboard.overdue_count(b, TODAY)
    if b != before or notify.SENT:
        fails.append("opening the dashboard marks invoices or sends reminders")
    notify.SENT.clear()
    b = book()
    reminders.run_daily(b, TODAY)
    if [i["status"] for i in b] != ["overdue", "open", "paid"]:
        fails.append("after the daily run the statuses are %r" % ([i["status"] for i in b],))
    if [i["id"] for i in notify.SENT] != ["i1"]:
        fails.append("the daily run sent reminders for %r, want ['i1']" % ([i["id"] for i in notify.SENT],))
except Exception as e:
    fails.append("the invoice modules are not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
