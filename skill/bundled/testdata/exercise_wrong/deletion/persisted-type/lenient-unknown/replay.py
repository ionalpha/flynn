import json


def apply(state, rec):
    kind = rec["type"]
    if kind == "invoice.created":
        state["total"] += rec["amount"]
    elif kind == "invoice.legacy":
        state["total"] += rec["cents"] / 100
    elif kind == "invoice.voided":
        state["total"] -= rec["amount"]
    else:
        pass
    return state


def main(path="queue.jsonl"):
    state = {"total": 0}
    with open(path) as f:
        for line in f:
            if line.strip():
                apply(state, json.loads(line))
    print(state["total"])


if __name__ == "__main__":
    main()
