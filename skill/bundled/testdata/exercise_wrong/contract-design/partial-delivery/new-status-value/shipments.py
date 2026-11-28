"""The webhook body partners receive when a shipment changes."""


def status_for(shipment):
    if shipment["returned"]:
        return "returned"
    if all(line["delivered_qty"] >= line["qty"] for line in shipment["lines"]):
        return "delivered"
    if is_partial(shipment):
        return "partially_delivered"
    if shipment["dispatched"]:
        return "in_transit"
    return "pending"


def is_partial(shipment):
    """Some units have arrived and some have not."""
    got = [line["delivered_qty"] for line in shipment["lines"]]
    return any(got) and not shipment["returned"] and shipment["dispatched"]


def shipment_payload(shipment):
    payload = {
        "id": shipment["id"],
        "status": status_for(shipment),
        "carrier": shipment["carrier"],
        "eta": shipment["eta"],
    }
    return payload
