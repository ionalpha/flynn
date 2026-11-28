from urllib.parse import quote

import http_util

BASE = "https://crm.example.test/v2/customers"
PAGE = 50


def sync_customers():
    latest = {}
    cursor = None
    while True:
        url = "%s?limit=%d" % (BASE, PAGE)
        if cursor is not None:
            url += "&cursor=" + quote(cursor, safe="")
        page = http_util.get_json(url)
        for r in page["data"]:
            latest[r["id"]] = r
        cursor = page["next_cursor"]
        if cursor is None:
            return list(latest.values())
