"""Vendored HTTP helper. Upstream names, do not rename."""


class HttpClient:
    def __init__(self, base_url):
        self.base_url = base_url
