# jobs

A job is a `JobConfig` handed to `jobs.enqueue`. The worker only ever reads the
queue, and it assumes every config in it is valid: a config with an empty queue
name or a priority outside 0 to 9 is never picked up and never reported.

Jobs come in through `api.submit_job` (HTTP), `cli.main` (operators), `cron.run_due`
(schedules), `webhooks.on_event` (partners) and `retry_failed.requeue` (moving a
failed job to another queue). `submit_job`, `run_due` and `on_event` raise
`ValueError` on a bad config; `cli.main` returns an exit code.
