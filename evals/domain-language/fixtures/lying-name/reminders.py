"""The daily invoice job."""

from invoices import is_overdue


def run_daily(invoices, today):
    """Return the ids of overdue invoices, for the job log."""
    return [invoice["id"] for invoice in invoices if is_overdue(invoice, today)]
