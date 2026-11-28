"""JSON GET with the retry policy every CRM call shares."""

import json
import time

import transport

MAX_ATTEMPTS = 3
UNAVAILABLE = (502, 503, 504)


class ProviderError(Exception):
    def __init__(self, status, message):
        super().__init__("CRM answered %s: %s" % (status, message))
        self.status = status


def get_json(url):
    status = None
    for attempt in range(MAX_ATTEMPTS):
        resp = transport.get(url, {"Accept": "application/json"})
        if resp.status in UNAVAILABLE:
            status = resp.status
            time.sleep(0.5 * 2**attempt)
            continue
        if resp.status != 200:
            raise ProviderError(resp.status, resp.body[:200])
        return json.loads(resp.body)
    raise ProviderError(status, "unavailable after %d attempts" % MAX_ATTEMPTS)
