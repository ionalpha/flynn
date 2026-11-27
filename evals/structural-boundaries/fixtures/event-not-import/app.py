import events
from adapters import stock

events.subscribe("order_cancelled", stock.release)
