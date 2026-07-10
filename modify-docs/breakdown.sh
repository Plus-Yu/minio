#!/bin/bash
# Usage: ./breakdown.sh [operation] [method]
#   ./breakdown.sh s3.GetObject GET
#   ./breakdown.sh s3.PutObject PUT
set -euo pipefail
OP="${1:-s3.GetObject}"
METHOD="${2:-GET}"

TOKEN=$(~/go/bin/mc admin prometheus generate myminio 2>&1 | awk '/bearer_token/{print $2}')
LD_LIBRARY_PATH= curl -s -H "Authorization: Bearer $TOKEN" \
  localhost:9000/minio/metrics/v3/s3/breakdown | python3 -c "
import sys, re
OP = sys.argv[1]
METHOD = sys.argv[2]
data = {}
for line in sys.stdin:
    m = re.match(r'.*_(sum|count)\{method=\"(\w+)\",operation=\"([^\"]+)\",phase=\"([^\"]+)\"\}\s+([\d.e+\-]+)', line)
    if not m: continue
    typ, method, op, phase, val = m.group(1), m.group(2), m.group(3), m.group(4), float(m.group(5))
    if op != OP or method != METHOD: continue
    k = (phase, op, method)
    if k not in data: data[k] = {'sum':0,'count':0}
    data[k][typ] = val

print(f'{\"phase\":<14} {\"avg(us)\":>10}  {\"%\":>6}  {\"count\":>8}')
print('-' * 44)
total_us = sum(v['sum']/v['count']*1e6 for v in data.values() if v['count'] > 0)
for (phase, op, method), v in sorted(data.items()):
    if v['count'] > 0:
        avg = v['sum']/v['count']*1e6
        pct = avg/total_us*100 if total_us > 0 else 0
        print(f'{phase:<14} {avg:>10.1f}  {pct:>5.1f}%  {v[\"count\"]:>8.0f}')
print('-' * 44)
print(f'{\"total\":<14} {total_us:>10.1f}us')
" "$OP" "$METHOD"
