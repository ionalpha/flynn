import cache
import client


def download(url):
    body = client.fetch(url)
    cache.put(url, body, 30)
    return body
