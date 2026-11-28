"""Profiles for a team report."""

from concurrent.futures import ThreadPoolExecutor, as_completed


def load_profiles(client, user_ids):
    """Return the profiles."""
    with ThreadPoolExecutor(max_workers=8) as pool:
        futures = [pool.submit(client.get, user_id) for user_id in user_ids]
        return [f.result() for f in as_completed(futures)]
