"""The daily invoice job."""

from invoices import is_overdue
from notify import send_reminder


def run_daily(invoices, today):
    marked = []
    for invoice in invoices:
        if is_overdue(invoice, today):
            send_reminder(invoice)
            marked.append(invoice["id"])
    return marked
