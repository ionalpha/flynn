"""Invoices and their due dates."""


def is_overdue(invoice, today):
    overdue = not invoice["paid"] and invoice["due"] < today
    if overdue:
        invoice["status"] = "overdue"
    return overdue
