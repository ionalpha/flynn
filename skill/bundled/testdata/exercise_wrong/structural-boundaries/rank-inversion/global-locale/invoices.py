import customers
import money
import pricing


def render(invoice):
    who = customers.get(invoice["customer_id"])
    money.set_locale(who["locale"])
    net = sum(line["cents"] for line in invoice["lines"])
    vat = pricing.vat_cents(net)
    out = ["Invoice %s for %s" % (invoice["id"], who["name"])]
    for line in invoice["lines"]:
        out.append("%s: %s" % (line["desc"], money.fmt(line["cents"])))
    out.append("VAT %d%%: %s" % (pricing.VAT_PERCENT, money.fmt(vat)))
    out.append("Total: %s" % money.fmt(net + vat))
    return out
