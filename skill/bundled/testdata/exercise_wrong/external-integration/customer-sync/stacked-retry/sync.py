import time
from urllib.parse import quote

import http_util

BASE = "https://crm.example.test/v2/customers"
PAGE = 50


def _fetch(url):
    for attempt in range(3):
        try:
            return http_util.get_json(url)
        except http_util.ProviderError:
            if attempt == 2:
                raise
            time.sleep(1)


def sync_customers():
    latest = {}
    cursor = None
    while True:
        url = "%s?limit=%d" % (BASE, PAGE)
        if cursor is not None:
            url += "&cursor=" + quote(cursor, safe="")
        page = _fetch(url)
        for r in page["data"]:
            seen = latest.get(r["id"])
            if seen is None or r["version"] > seen["version"]:
                latest[r["id"]] = r
        cursor = page["next_cursor"]
        if cursor is None:
            return list(latest.values())
