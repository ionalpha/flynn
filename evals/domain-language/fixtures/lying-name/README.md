# invoicing

The scheduler calls `reminders.run_daily(invoices, today)` once a day. The finance
dashboard calls `dashboard.overdue_count(invoices, today)` every time someone opens it.
`notify.send_reminder(invoice)` emails the customer.
