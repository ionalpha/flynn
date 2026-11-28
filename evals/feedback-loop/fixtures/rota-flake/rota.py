"""On-call rota for the support team."""


def build_rota(people, days, rng, away=None):
    """One person per day, never someone who is away that day and never the same person twice in a row."""
    away = away or {}
    counts = {p: 0 for p in people}
    rota = []
    for day in range(days):
        prev = rota[-1] if rota else None
        pool = [p for p in people if p != prev and day not in away.get(p, ())]
        fewest = min(counts[p] for p in pool)
        pick = rng.choice([p for p in pool if counts[p] == fewest])
        counts[pick] += 1
        rota.append(pick)
    return rota
