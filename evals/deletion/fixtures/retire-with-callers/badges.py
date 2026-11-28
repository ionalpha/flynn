import slugs


def badge_id(label):
    return "badge-" + slugs.slugify(label)
