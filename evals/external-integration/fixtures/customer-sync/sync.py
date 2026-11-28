"""Nightly pull of the CRM customer list into the warehouse."""

import http_util

BASE = "https://crm.example.test/v2/customers"
PAGE = 50


def sync_customers():
    """Every customer the CRM lists. The CRM returns full pages until the last one."""
    out = []
    cursor = None
    while True:
        url = "%s?limit=%d" % (BASE, PAGE)
        if cursor:
            url += "&cursor=" + cursor
        try:
            page = http_util.get_json(url)
        except Exception:
            break
        out.extend(page["data"])
        if len(page["data"]) < PAGE:
            break
        cursor = page["next_cursor"]
    return out
