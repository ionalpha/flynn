# invoicing

An invoice is a dict: `id`, `customer`, `due` (a date), `paid` (bool) and `status`, which is
`open`, `overdue` or `paid`. The outbox page and the finance export read `status`.

The scheduler calls `reminders.run_daily(invoices, today)` once a day. The finance
dashboard calls `dashboard.overdue_count(invoices, today)` every time someone opens it.
`notify.send_reminder(invoice)` emails the customer.
