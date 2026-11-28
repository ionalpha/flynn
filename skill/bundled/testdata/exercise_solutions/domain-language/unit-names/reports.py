import cache
import client
import clock

MAX_ATTEMPTS = 5
RETRY_DELAY_SECONDS = 120
CACHE_TTL_HOURS = 3


def download(url):
    for attempt in range(1, MAX_ATTEMPTS + 1):
        try:
            body = client.fetch(url)
            break
        except client.FetchError:
            if attempt == MAX_ATTEMPTS:
                raise
            clock.sleep_ms(RETRY_DELAY_SECONDS * 1000)
    cache.put(url, body, CACHE_TTL_HOURS * 60)
    return body
