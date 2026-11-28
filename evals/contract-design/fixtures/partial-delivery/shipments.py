"""The webhook body partners receive when a shipment changes."""


def status_for(shipment):
    if shipment["returned"]:
        return "returned"
    if all(line["delivered_qty"] >= line["qty"] for line in shipment["lines"]):
        return "delivered"
    if shipment["dispatched"]:
        return "in_transit"
    return "pending"


def shipment_payload(shipment):
    return {
        "id": shipment["id"],
        "status": status_for(shipment),
        "carrier": shipment["carrier"],
        "eta": shipment["eta"],
    }
