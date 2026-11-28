"""Invoices and their due dates."""

from notify import send_reminder


def is_overdue(invoice, today):
    overdue = not invoice["paid"] and invoice["due"] < today
    if overdue and invoice["status"] != "overdue":
        invoice["status"] = "overdue"
        send_reminder(invoice)
    return overdue
