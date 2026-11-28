import pages
from web.cache import PageCache

CACHE = PageCache()
ROUTES = {"product": CACHE.wrap(pages.product_page)}


def get(route, *args):
    return ROUTES[route](*args)
