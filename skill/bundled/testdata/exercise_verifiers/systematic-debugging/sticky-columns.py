import sys
sys.path.insert(0, ".")
fails = []
try:
    from export import export
    row = {"id": 1, "date": "d", "total": 2, "note": "n", "vat": "v", "po": "p"}
    nl = chr(10)
    cases = [
        ({"extra": ["vat"]}, "id,date,total,note,vat" + nl + "1,d,2,n,v"),
        ({"extra": ["po"], "hidden": ["note"]}, "id,date,total,po" + nl + "1,d,2,p"),
        ({}, "id,date,total,note" + nl + "1,d,2,n"),
        ({"hidden": ["total"], "delimiter": ";"}, "id;date;note" + nl + "1;d;n"),
        ({"extra": ["vat"]}, "id,date,total,note,vat" + nl + "1,d,2,n,v"),
        ({}, "id,date,total,note" + nl + "1,d,2,n"),
    ]
    for i, (tenant, want) in enumerate(cases):
        got = export(tenant, [row])
        if got != want:
            fails.append("export %d for %r gave %r, want %r" % (i + 1, tenant, got, want))
except Exception as e:
    fails.append("export could not be exercised: %r" % e)
for x in fails:
    print(x, file=sys.stderr)
sys.exit(1 if fails else 0)
