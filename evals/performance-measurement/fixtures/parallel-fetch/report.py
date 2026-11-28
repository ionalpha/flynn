"""Profiles for a team report."""


def load_profiles(client, user_ids):
    """Return one profile per id, in the order given."""
    return [client.get(user_id) for user_id in user_ids]
