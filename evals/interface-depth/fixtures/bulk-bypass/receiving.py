def book_delivery(inventory, lines):
    for sku, qty in lines.items():
        inventory.adjust(sku, qty, "delivery")
