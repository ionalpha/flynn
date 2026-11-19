"""Client for the reports service. PROVIDER.md has what it returns."""

import json

import transport

BASE = "https://reports.example.com/v2"


class ReportError(Exception):
    def __init__(self, status, body):
        super().__init__(f"reports service answered {status}")
        self.status = status
        self.body = body


def fetch_report(report_id):
    resp = transport.get(f"{BASE}/reports/{report_id}")
    if resp.status != 200:
        raise ReportError(resp.status, resp.body)
    return json.loads(resp.body)
