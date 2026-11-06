#!/bin/bash
# Compatibility smoke test: one single-node cluster of any supported
# Elasticsearch/OpenSearch version, exercised through every page and the
# common write paths. CI runs it once per version in the support matrix.
#
#   ES=http://localhost:9200 FLAVOR=es9 ./scripts/compat.sh
#
# Medulla must be running with scripts/compat.config.yaml (cluster "compat"
# at $ES, plus "dead", which nothing answers).
M=${M:-http://localhost:8080}
ES=${ES:-http://localhost:9200}
FLAVOR=${FLAVOR:?set FLAVOR to the expected flavor, e.g. es8 or opensearch}
C=$M/c/compat
D=$(mktemp -d); W=$D/w.jar; R=$D/r.jar
P=0; F=0
ok()   { P=$((P+1)); echo "ok:   $1"; }
fail() { F=$((F+1)); echo "FAIL: $1"; }
check() { d="$1"; want="$2"; shift 2
  got=$(curl -s -o /dev/null -w '%{http_code}' "$@")
  [ "$got" = "$want" ] && ok "$d" || fail "$d (got $got want $want)"; }
has()  { curl -s -b $W "$2" | grep -q -- "$3" && ok "$1" || fail "$1"; }
hasnt() { curl -s -b $W "$2" | grep -q -- "$3" && fail "$1" || ok "$1"; }

check "writer login" 303 -c $W -d 'username=writer&password=writerpw' $M/login
check "reader login" 303 -c $R -d 'username=reader&password=readerpw' $M/login

## flavor and version detected
has "flavor is $FLAVOR" "$C/overview" "<span class=\"v\">$FLAVOR "

## per-node version column (rolling-upgrade visibility) on this version too
V=$(curl -s "$ES/" | grep -o '"number" *: *"[^"]*"' | head -1 | sed 's/.*"\([^"]*\)"$/\1/')
has "node version column shows $V" "$C/overview?sort=version" "<td class=\"\">$V</td>"
hasnt "single version: no mixed banner" "$C/overview" 'Mixed versions'

## every read page, including sorted/filtered variants
for p in "" "?layout=list" "?layout=list&sort=docs" "?health=unreachable" "?q=comp" "?refresh=10"; do
  check "GET /$p" 200 -b $W "$M/$p"
done
for p in overview "overview?sort=cpu&refresh=30" indices "indices?sort=size" "indices?health=green" \
         aliases templates snapshots analyze settings console \
         cat/shards "cat/shards?sort=store&dir=desc" cat/indices cat/nodes "cat/nodes?q=compat" cat/health; do
  check "GET $p" 200 -b $W "$C/$p"
done

## landing page: live cluster healthy, dead one classified, problems first
LIST=$(curl -s -b $W "$M/?layout=list")
echo "$LIST" | grep -q 'badge health-red">refused<' && ok "dead cluster classified as refused" || fail "dead cluster not classified"
[ "$(echo "$LIST" | grep -o 'href="/c/[a-z]*/overview"' | head -1)" = 'href="/c/dead/overview"' ] \
  && ok "unreachable listed first" || fail "unreachable not first"
echo "$LIST" | grep -q 'class="dot \(green\|yellow\)"' && ok "live cluster has health" || fail "live cluster health missing"

## writes, each visible on the very next read (cache cleared by the write)
check "create index" 303 -b $W -d 'name=compat-a&shards=2&replicas=0' $C/indices
has "new index listed immediately" "$C/indices" '>compat-a<'
has "new index in shard matrix" "$C/overview?q=compat-a" 'href="/c/compat/indices/compat-a"'
has "new index in _cat immediately" "$C/cat/indices?q=compat-a" 'compat-a'
check "close index" 303 -b $W -X POST $C/indices/compat-a/close
check "open index" 303 -b $W -X POST $C/indices/compat-a/open
check "index setting put" 303 -b $W -d 'key=index.refresh_interval&value=30s' $C/indices/compat-a/settings
curl -s "$ES/compat-a/_settings?flat_settings=true" | grep -q '"index.refresh_interval":"30s"' \
  && ok "index setting reached cluster" || fail "index setting didn't land"
check "alias add" 303 -b $W -d 'action=add&index=compat-a&alias=compat-alias' $C/aliases
has "alias listed" "$C/aliases" 'compat-alias'
check "template create" 303 -b $W --data-urlencode 'name=compat-tpl' --data-urlencode 'body={"index_patterns":["compat-x-*"]}' $C/templates
has "template listed" "$C/templates" 'compat-tpl'
check "template delete" 303 -b $W -X POST $C/templates/compat-tpl/delete

## routing: restriction shows at once, and clears
check "allocation primaries" 303 -b $W -d 'kind=allocation&value=primaries' $C/routing
has "restriction banner shown" "$C/overview" 'Shard routing is restricted'
check "allocation all" 303 -b $W -d 'kind=allocation&value=all' $C/routing
check "setting reset" 303 -b $W -d 'key=cluster.routing.allocation.enable&value=' $C/settings
hasnt "restriction cleared" "$C/overview" 'Shard routing is restricted'

## console + analyze
curl -s -b $W -d 'method=GET&path=/_cluster/health' $C/console | grep -q 'cluster_name' && ok "console GET" || fail "console GET"
curl -s -b $W -d 'analyzer=standard&text=hello+world' $C/analyze | grep -q '<td>hello</td>' && ok "analyze" || fail "analyze"

## RBAC holds on this version too
check "reader create denied" 403 -b $R -d 'name=nope' $C/indices
check "reader routing denied" 403 -b $R -d 'kind=allocation&value=none' $C/routing

## cleanup: unconfirmed delete bounces, confirmed delete lands
check "delete unconfirmed bounces" 303 -b $W -d 'confirm=wrong' $C/indices/compat-a/delete
curl -sf -o /dev/null "$ES/compat-a" && ok "unconfirmed delete did nothing" || fail "index deleted without confirm"
check "delete confirmed" 303 -b $W -d 'confirm=compat-a' $C/indices/compat-a/delete
hasnt "deleted index gone immediately" "$C/indices" '>compat-a<'

echo
echo "passed=$P failed=$F"
[ $F -eq 0 ]
