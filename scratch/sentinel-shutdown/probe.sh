#!/bin/sh
# probe.sh "<redis ips>" "<sentinel ips>"
# One line per redis: R ip role master_host link offset
# One line per sentinel: S ip master
for ip in $1; do
  timeout 2 redis-cli -h "$ip" info replication 2>/dev/null | tr -d '\r' |
    awk -F: -v ip="$ip" '/^role:/{r=$2} /^master_host:/{h=$2} /^master_link_status:/{l=$2} /^master_repl_offset:/{o=$2}
      END{if(r=="")r="down"; if(h=="")h="-"; if(l=="")l="-"; if(o=="")o="-"; printf "R %s %s %s %s %s\n", ip, r, h, l, o}'
done
for ip in $2; do
  printf 'S %s %s\n' "$ip" "$(timeout 2 redis-cli -h "$ip" -p 26379 sentinel get-master-addr-by-name mymaster 2>/dev/null | head -1)"
done
