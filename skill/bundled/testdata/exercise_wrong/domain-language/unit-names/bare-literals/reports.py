import cache
import client
import clock

MAX_ATTEMPTS = 5


def download(url):
    for attempt in range(1, MAX_ATTEMPTS + 1):
        try:
            body = client.fetch(url)
            break
        except client.FetchError:
            if attempt == MAX_ATTEMPTS:
                raise
            clock.sleep_ms(120000)
    cache.put(url, body, 180)
    return body
