import cache
import client
import clock

MAX_ATTEMPTS = 5
RETRY_DELAY = 120000
CACHE_TTL = 180


def download(url):
    for attempt in range(1, MAX_ATTEMPTS + 1):
        try:
            body = client.fetch(url)
            break
        except client.FetchError:
            if attempt == MAX_ATTEMPTS:
                raise
            clock.sleep_ms(RETRY_DELAY)
    cache.put(url, body, CACHE_TTL)
    return body
