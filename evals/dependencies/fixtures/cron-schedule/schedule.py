"""When a scheduled report next runs."""

from dataclasses import dataclass


@dataclass
class Report:
    name: str
    cron: str


def next_run(expr, after):
    """First minute strictly after `after` at which the cron expression fires."""
    raise NotImplementedError("not written yet")


def due_after(report, after):
    return next_run(report.cron, after)


def describe(report, after):
    when = due_after(report, after)
    return "%s next runs %s" % (report.name, when.strftime("%Y-%m-%d %H:%M"))
