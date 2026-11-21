"""Tag handling for the article editor."""


def normalise(tags):
    """Distinct tags, stripped and lower-cased, in alphabetical order."""
    return sorted({t.strip().lower() for t in tags if t.strip()})
