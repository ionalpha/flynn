"""Text the dashboard shows for a shipment status."""

LABELS = {
    "pending": "Waiting to ship",
    "in_transit": "On its way",
    "delivered": "Delivered",
    "returned": "Sent back",
}


def label(status):
    return LABELS[status]
