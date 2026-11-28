from slugs import slugify_v1


def export_name(title, ext):
    return "%s.%s" % (slugify_v1(title), ext)
