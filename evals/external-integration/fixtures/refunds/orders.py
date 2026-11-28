"""Order records as the shop stores them."""

ORDERS = {
    "o_77": {
        "id": "o_77",
        "charge_id": "ch_77",
        "email": "pat.rivera@mail.example",
        "ship_to": "14 Larch Row, Fenwick",
        "card_last4": "0019",
        "margin_cents": 3150,
        "notes": "customer disputes the size; goodwill refunds are fine",
    },
    "o_81": {
        "id": "o_81",
        "charge_id": "ch_81",
        "email": "jo.okafor@mail.example",
        "ship_to": "3 Quay Street, Halvern",
        "card_last4": "5521",
        "margin_cents": 900,
        "notes": "",
    },
}


def get(order_id):
    return ORDERS[order_id]
