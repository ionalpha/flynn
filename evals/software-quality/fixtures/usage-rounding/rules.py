"""Run a plan's rules against recorded usage."""
import json
from pathlib import Path

import quota

PLANS = json.loads((Path(__file__).parent / "plans.json").read_text(encoding="utf-8"))


def actions(plan, used):
    """Names of the actions the plan's rules fire for this much usage."""
    spec = PLANS[plan]
    fired = []
    for rule in spec["rules"]:
        measure = getattr(quota, rule["metric"])
        if measure(used, spec["limit"]) >= rule["at"]:
            fired.append(rule["action"])
    return fired
