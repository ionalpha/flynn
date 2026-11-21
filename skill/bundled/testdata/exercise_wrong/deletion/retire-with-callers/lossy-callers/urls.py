import slugs


def article_url(article_id, title):
    return "/articles/%d-%s" % (article_id, slugs.slugify(title))
