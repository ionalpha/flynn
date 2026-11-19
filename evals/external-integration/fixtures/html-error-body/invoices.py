"""Client for the invoices service. PROVIDER.md has what it returns."""

import json

import transport

BASE = "https://billing.example.com/v1"


class ApiError(Exception):
    pass


def get_invoice(invoice_id):
    resp = transport.get(f"{BASE}/invoices/{invoice_id}")
    data = json.loads(resp.body)
    if "error" in data:
        raise ApiError(data["error"]["message"])
    return data
