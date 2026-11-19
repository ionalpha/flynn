"""The daily invoice job."""

from invoices import is_overdue
from notify import send_reminder


def run_daily(invoices, today):
    """Mark newly overdue invoices and remind their customers once.

    Return the ids marked, for the job log.
    """
    marked = []
    for invoice in invoices:
        if invoice["status"] != "overdue" and is_overdue(invoice, today):
            invoice["status"] = "overdue"
            send_reminder(invoice)
            marked.append(invoice["id"])
    return marked
