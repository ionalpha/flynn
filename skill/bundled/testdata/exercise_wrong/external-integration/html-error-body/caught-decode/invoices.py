"""Client for the invoices service. PROVIDER.md has what it returns."""

import json

import transport

BASE = "https://billing.example.com/v1"


class ApiError(Exception):
    pass


def get_invoice(invoice_id):
    resp = transport.get(f"{BASE}/invoices/{invoice_id}")
    try:
        data = json.loads(resp.body)
    except ValueError:
        raise ApiError("the invoices service sent an unexpected response") from None
    if "error" in data:
        raise ApiError(data["error"]["message"])
    return data
