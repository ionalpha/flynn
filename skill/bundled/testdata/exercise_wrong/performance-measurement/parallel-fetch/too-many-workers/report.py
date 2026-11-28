"""Profiles for a team report."""

from concurrent.futures import ThreadPoolExecutor


def load_profiles(client, user_ids):
    """Return one profile per id, in the order given."""
    with ThreadPoolExecutor(max_workers=50) as pool:
        return list(pool.map(client.get, user_ids))
