"""Text the dashboard shows for a shipment status."""

LABELS = {
    "pending": "Waiting to ship",
    "in_transit": "On its way",
    "in_transit_partial": "Partly delivered",
    "delivered": "Delivered",
    "returned": "Sent back",
}


def label(status):
    return LABELS[status]
