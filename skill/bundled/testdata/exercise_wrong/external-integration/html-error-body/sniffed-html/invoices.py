"""Client for the invoices service. PROVIDER.md has what it returns."""

import json

import transport

BASE = "https://billing.example.com/v1"


class ApiError(Exception):
    pass


def get_invoice(invoice_id):
    resp = transport.get(f"{BASE}/invoices/{invoice_id}")
    if resp.body.lstrip().startswith("<"):
        ctype = resp.headers.get("Content-Type", "")
        raise ApiError(f"{resp.status} {ctype}: {resp.body.splitlines()[0]}")
    data = json.loads(resp.body)
    if "error" in data:
        raise ApiError(data["error"]["message"])
    return data
