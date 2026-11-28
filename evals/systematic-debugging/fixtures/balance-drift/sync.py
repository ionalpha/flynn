"""Apply payment events to customer balances."""

import json
import os


def parse_amount(text):
    """Return an amount in cents."""
    return int(round(float(text.split(",")[0]) * 100))


def run(directory="."):
    """Apply the events not yet applied and save the balances."""
    state_path = os.path.join(directory, "balances.json")
    if os.path.exists(state_path):
        with open(state_path) as f:
            state = json.load(f)
    else:
        with open(os.path.join(directory, "opening.json")) as f:
            state = {"applied": 0, "balances": json.load(f)}

    with open(os.path.join(directory, "events.jsonl")) as f:
        events = [json.loads(line) for line in f if line.strip()]

    for event in events[state["applied"]:]:
        customer = event["customer"]
        cents = parse_amount(event["amount"])
        state["balances"][customer] = state["balances"].get(customer, 0) + cents
    state["applied"] = len(events)

    with open(state_path, "w") as f:
        json.dump(state, f, indent=2, sort_keys=True)
        f.write("\n")


if __name__ == "__main__":
    run()
