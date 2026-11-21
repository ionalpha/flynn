"""Numbers for the finance dashboard."""

from invoices import is_overdue


def overdue_count(invoices, today):
    return sum(1 for invoice in invoices if is_overdue(invoice, today))
