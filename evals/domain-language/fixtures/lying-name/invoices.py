"""Invoices and their due dates."""


def is_overdue(invoice, today):
    return not invoice["paid"] and invoice["due"] < today
