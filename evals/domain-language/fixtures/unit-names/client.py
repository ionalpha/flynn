TRANSPORT = None


class FetchError(Exception):
    pass


def fetch(url):
    return TRANSPORT(url)
