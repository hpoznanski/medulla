#!/bin/bash
# E2E test against the running dev stack — exercises every UI endpoint.
M=http://localhost:8080
W=/tmp/e2e-writer.jar; R=/tmp/e2e-reader.jar
P=0; F=0
ok()   { P=$((P+1)); echo "ok:   $1"; }
fail() { F=$((F+1)); echo "FAIL: $1"; }
# check <desc> <want-code> <curl args...>
check() { d="$1"; want="$2"; shift 2
  got=$(curl -s -o /dev/null -w '%{http_code}' "$@")
  [ "$got" = "$want" ] && ok "$d" || fail "$d (got $got want $want)"; }

## auth
check "login page" 200 $M/login
check "writer login" 303 -c $W -d 'username=writer&password=writerpw' $M/login
check "reader login" 303 -c $R -d 'username=reader&password=readerpw' $M/login
check "bad password rejected (login page again)" 200 -d 'username=writer&password=nope' $M/login
check "unauthenticated overview -> login" 303 $M/c/dev-es/overview

## all GET pages, both clusters, as writer
for c in dev-es dev-opensearch; do
  for p in overview indices aliases templates snapshots analyze settings cat/shards cat/indices cat/nodes console; do
    check "GET /c/$c/$p (writer)" 200 -b $W $M/c/$c/$p
  done
done
check "clusters landing" 200 -b $W $M/

## write ops on dev-es as writer
check "create index" 303 -b $W -d 'name=e2e-test&shards=2&replicas=0' $M/c/dev-es/indices
sleep 1
curl -s -b $W $M/c/dev-es/indices | grep -q 'e2e-test' && ok "index visible in UI" || fail "index missing in UI"
check "refresh index" 303 -b $W -X POST $M/c/dev-es/indices/e2e-test/refresh
check "flush index" 303 -b $W -X POST $M/c/dev-es/indices/e2e-test/flush
check "forcemerge index" 303 -b $W -X POST $M/c/dev-es/indices/e2e-test/forcemerge
check "close index" 303 -b $W -X POST $M/c/dev-es/indices/e2e-test/close
check "open index" 303 -b $W -X POST $M/c/dev-es/indices/e2e-test/open
check "index detail page" 200 -b $W $M/c/dev-es/indices/e2e-test

## aliases
check "alias add" 303 -b $W -d 'action=add&index=e2e-test&alias=e2e-alias' $M/c/dev-es/aliases
curl -s http://localhost:9200/_cat/aliases | grep -q e2e-alias && ok "alias exists in ES" || fail "alias not in ES"
check "alias remove" 303 -b $W -d 'action=remove&index=e2e-test&alias=e2e-alias' $M/c/dev-es/aliases

## templates
check "template create" 303 -b $W --data-urlencode 'name=e2e-tpl' --data-urlencode 'body={"index_patterns":["e2e-x-*"]}' $M/c/dev-es/templates
curl -s http://localhost:9200/_index_template/e2e-tpl | grep -q e2e-x && ok "template exists in ES" || fail "template not in ES"
check "template delete" 303 -b $W -X POST $M/c/dev-es/templates/e2e-tpl/delete

## snapshots (repo backup exists from earlier; recreate to be safe)
check "repo create" 303 -b $W --data-urlencode 'reponame=e2e-repo' --data-urlencode 'body={"type":"fs","settings":{"location":"/backup/e2e"}}' $M/c/dev-es/snapshots/repo-create
check "snapshot create" 303 -b $W -d 'repo=e2e-repo&name=e2e-snap' $M/c/dev-es/snapshots/create
sleep 2
curl -s -b $W "$M/c/dev-es/snapshots?repo=e2e-repo" | grep -q 'e2e-snap' && ok "snapshot listed in UI" || fail "snapshot missing"
check "snapshot delete unconfirmed bounces" 303 -b $W -d 'repo=e2e-repo&name=e2e-snap&confirm=wrong' $M/c/dev-es/snapshots/delete
curl -s http://localhost:9200/_snapshot/e2e-repo/e2e-snap | grep -q '"snapshot":"e2e-snap"' && ok "unconfirmed delete did nothing" || fail "snapshot deleted without confirm"
check "snapshot delete confirmed" 303 -b $W -d 'repo=e2e-repo&name=e2e-snap&confirm=e2e-snap' $M/c/dev-es/snapshots/delete

## cluster settings
check "setting put" 303 -b $W -d 'key=cluster.routing.allocation.enable&value=all' $M/c/dev-es/settings
curl -s -b $W $M/c/dev-es/settings | grep -q 'cluster.routing.allocation.enable' && ok "setting visible" || fail "setting missing"
check "setting reset" 303 -b $W -d 'key=cluster.routing.allocation.enable&value=' $M/c/dev-es/settings

## analyze + console
curl -s -b $W -d 'analyzer=standard&text=hello+world' $M/c/dev-es/analyze | grep -q '<td>hello</td>' && ok "analyze returns tokens" || fail "analyze broken"
curl -s -b $W -d 'method=GET&path=/_cluster/health' $M/c/dev-es/console | grep -q 'cluster_name' && ok "console GET works" || fail "console GET broken"
check "console PUT (writer, rest:full)" 200 -b $W -d 'method=PUT&path=/e2e-console-idx' $M/c/dev-es/console
curl -s http://localhost:9200/e2e-console-idx | grep -q e2e-console-idx && ok "console PUT reached ES" || fail "console PUT didn't land"

## RBAC: reader denied everywhere it should be
check "reader sees overview" 200 -b $R $M/c/dev-es/overview
check "reader console denied" 403 -b $R $M/c/dev-es/console
check "reader create index denied" 403 -b $R -d 'name=nope' $M/c/dev-es/indices
check "reader index action denied" 403 -b $R -X POST $M/c/dev-es/indices/e2e-test/refresh
check "reader alias denied" 403 -b $R -d 'action=add&index=x&alias=y' $M/c/dev-es/aliases
check "reader template denied" 403 -b $R -d 'name=x&body={}' $M/c/dev-es/templates
check "reader snapshot denied" 403 -b $R -d 'repo=x&name=y' $M/c/dev-es/snapshots/create
check "reader settings denied" 403 -b $R -d 'key=x&value=y' $M/c/dev-es/settings
curl -s -b $R $M/c/dev-es/indices | grep -q 'Create index' && fail "reader sees write UI" || ok "reader sees no write controls"

## cross-origin + traversal
check "cross-origin POST rejected" 403 -H 'Origin: https://evil.example' -X POST $M/login
curl -s -b $W -d 'method=GET&path=/%2e%2e/x' $M/c/dev-es/console | grep -q 'invalid path' && ok "encoded traversal rejected" || fail "traversal accepted"

## cleanup
check "delete e2e index (confirmed)" 303 -b $W -d 'confirm=e2e-test' $M/c/dev-es/indices/e2e-test/delete
check "delete console idx" 200 -b $W -d 'method=DELETE&path=/e2e-console-idx' $M/c/dev-es/console
curl -s -XDELETE http://localhost:9200/_snapshot/e2e-repo >/dev/null

echo; echo "passed=$P failed=$F"
[ $F -eq 0 ]
