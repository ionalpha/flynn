from slugs import slugify


def export_name(title, ext):
    return "%s.%s" % (slugify(title), ext)
