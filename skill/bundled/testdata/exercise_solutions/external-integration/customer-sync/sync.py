"""Nightly pull of the CRM customer list into the warehouse."""

from urllib.parse import quote

import http_util

BASE = "https://crm.example.test/v2/customers"
PAGE = 50


def sync_customers():
    """Every customer the CRM lists, ending only when next_cursor is null.

    A page can be short or empty mid-listing, a customer can appear on two pages (the
    higher version is current), and a failed page raises so a partial list is never
    taken for a finished sync.
    """
    latest = {}
    cursor = None
    while True:
        url = "%s?limit=%d" % (BASE, PAGE)
        if cursor is not None:
            url += "&cursor=" + quote(cursor, safe="")
        page = http_util.get_json(url)
        for r in page["data"]:
            seen = latest.get(r["id"])
            if seen is None or r["version"] > seen["version"]:
                latest[r["id"]] = r
        cursor = page["next_cursor"]
        if cursor is None:
            return list(latest.values())
