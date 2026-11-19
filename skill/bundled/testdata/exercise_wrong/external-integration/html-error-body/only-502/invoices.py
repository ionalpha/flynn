"""Client for the invoices service. PROVIDER.md has what it returns."""

import json

import transport

BASE = "https://billing.example.com/v1"


class ApiError(Exception):
    pass


def get_invoice(invoice_id):
    """Return the invoice, or raise ApiError naming the status.

    The status decides success. The body is detail, and it may be a page from the load
    balancer instead of the service, so a body that is not JSON is reported by its
    content type and first line.
    """
    resp = transport.get(f"{BASE}/invoices/{invoice_id}")
    ctype = resp.headers.get("Content-Type", "no content type")
    try:
        data = json.loads(resp.body)
    except ValueError:
        if resp.status != 502:
            raise ApiError(f"invoices service answered {resp.status}") from None
        first = resp.body.splitlines()[0][:200] if resp.body else ""
        raise ApiError(f"invoices service answered {resp.status} with {ctype} that is not JSON: {first!r}") from None
    if resp.status != 200:
        err = data.get("error") if isinstance(data, dict) else None
        message = err.get("message") if isinstance(err, dict) else err
        raise ApiError(f"invoices service answered {resp.status}: {message or data!r}")
    return data
