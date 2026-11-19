"""Client for the reports service. PROVIDER.md has what it returns."""

import json
import random

import clock
import transport

BASE = "https://reports.example.com/v2"


class ReportError(Exception):
    def __init__(self, status, body):
        super().__init__(f"reports service answered {status}")
        self.status = status
        self.body = body


# 503 is a deploy or an overload and 429 carries its own schedule; 401 and 404 fail
# the same way on every attempt, so they are raised at once.
RETRYABLE = {429, 502, 503, 504}
ATTEMPTS = 3
MAX_WAIT = 60


def _wait(resp, attempt):
    if resp.status == 429:
        try:
            return float(resp.headers.get("Retry-After", "1"))
        except ValueError:
            return 1.0
    return random.uniform(0, 0.5 * 2**attempt)


def fetch_report(report_id):
    for attempt in range(ATTEMPTS):
        resp = transport.get(f"{BASE}/reports/{report_id}")
        if resp.status == 200:
            return json.loads(resp.body)
        if resp.status not in RETRYABLE or attempt == ATTEMPTS - 1:
            raise ReportError(resp.status, resp.body)
        clock.sleep(_wait(resp, attempt))
