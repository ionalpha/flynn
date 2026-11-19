"""String helpers used across the site."""


def title_case(text):
    return " ".join(word[:1].upper() + word[1:] for word in text.split())
