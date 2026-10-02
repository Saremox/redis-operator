#!/usr/bin/env python3
"""Checks deploy/monitoring against a Prometheus that scraped the tester,
and imports the dashboard into Grafana.

Usage: check-monitoring.py PROMETHEUS_URL GRAFANA_URL RANGE_SECONDS RF...

Every alert, dashboard panel, template variable and annotation expression
must evaluate without error, and every metric it selects must be one the
tester defines.
A dashboard expression must also return data wherever the series it
selects have data; one that ends in a filter like `> 0` may return
nothing, but not without it. An alert returns nothing until it fires,
which e2e/rules-test.yaml tests, and none may have fired. Every rule must
load and evaluate healthy in Prometheus. Grafana must import the dashboard and run every panel query
through its Prometheus data source. Alerts that fired are listed. Exits 1
on any error.
"""
import base64
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

import yaml

prom, grafana, seconds, rfs = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4:]
here = os.path.dirname(os.path.abspath(__file__))
monitoring = os.path.join(here, "..", "deploy", "monitoring")
# The URLs are port-forwards on localhost: never through a proxy.
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
errors = []
SELECTOR = re.compile(r"\b(redis_soak_[a-z_]+)(\{[^}]*\})?")
FILTER = re.compile(r"^(.*\S)\s*(>|<|==|!=|>=|<=)\s*[0-9.]+\s*$", re.S)
RANGE = f"{seconds}s"
# Every metric the tester defines: a metric without series, like the
# version transitions of a profile without chains, is no error.
KNOWN = set()
for name in re.findall(r'Name:\s+"(\w+)"', open(os.path.join(here, "..", "internal", "metrics", "metrics.go")).read()):
    KNOWN |= {f"redis_soak_{name}" + suffix for suffix in ("", "_bucket", "_sum", "_count")}


def call(url, body=None, auth=None):
    req = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode())
    if body is not None:
        req.add_header("Content-Type", "application/json")
    if auth:
        req.add_header("Authorization", "Basic " + base64.b64encode(auth.encode()).decode())
    try:
        with opener.open(req, timeout=30) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        return e.code, json.load(e)


def query(expr):
    _, r = call(prom + "/api/v1/query?" + urllib.parse.urlencode({"query": expr}))
    if r.get("status") != "success":
        return None, r.get("error", r)
    return r["data"]["result"], None


def has_data(selector):
    result, err = query(f"count(last_over_time({selector}[{RANGE}]))")
    return err is None and len(result) > 0


def selectors_with_data(expr):
    """Returns whether every series the expression selects had data in the
    range, and notes on those that hadn't."""
    ok, notes = True, []
    for name, matchers in SELECTOR.findall(expr):
        selector = name + matchers
        if has_data(selector):
            continue
        ok = False
        if has_data(name):
            notes.append(f"no series match {selector} yet")
        elif name in KNOWN:
            notes.append(f"no {name} series in this profile")
        else:
            errors.append(f"{expr!r}: the tester has no metric {name}")
    return ok, notes


def check(what, expr, alert=False):
    result, err = query(expr)
    if err is not None:
        errors.append(f"{what}: {expr!r}: {err}")
        return "ERROR"
    data, notes = selectors_with_data(expr)
    if result:
        return f"{len(result)} series"
    if alert:
        return "not firing" + "".join("; " + n for n in notes)
    if not data:
        return "empty; " + "; ".join(notes)
    m = FILTER.match(expr)
    if m:
        base, err = query(m.group(1))
        if err is None and base:
            return f"empty, {len(base)} series before its filter"
    errors.append(f"{what}: {expr!r} returns nothing although its series have data")
    return "ERROR: empty"


def interpolate(expr, rf):
    return (expr.replace("$__rate_interval", "1m").replace("$__interval", "15s")
            .replace("$__range", RANGE).replace("$rf", rf))


print("--- rules loaded by Prometheus")
_, r = call(prom + "/api/v1/rules")
rules = [rule for g in r["data"]["groups"] for rule in g["rules"]]
for rule in rules:
    health = rule.get("health")
    print(f"{rule['name']}: health={health} state={rule.get('state')} lastError={rule.get('lastError', '')!r}")
    if health != "ok" or rule.get("lastError"):
        errors.append(f"rule {rule['name']} is {health}: {rule.get('lastError')}")
spec = yaml.safe_load(open(os.path.join(monitoring, "prometheusrule.yaml")))["spec"]
alerts = [rule for g in spec["groups"] for rule in g["rules"]]
if len(rules) != len(alerts):
    errors.append(f"Prometheus loaded {len(rules)} rules, the PrometheusRule has {len(alerts)}")

print("--- alert expressions")
for rule in alerts:
    expr = rule["expr"].strip()
    print(f"{rule['alert']}: {check(rule['alert'], expr, alert=True)}")

print("--- dashboard expressions, per instance")
dashboard = json.load(open(os.path.join(monitoring, "dashboard.json")))
targets = [(p["title"], t["expr"]) for p in dashboard["panels"] for t in p.get("targets", [])]
for title, expr in targets:
    for rf in rfs if "$rf" in expr else ["*"]:
        print(f"{title} [{rf}]: {check(title, interpolate(expr, rf))}")
for a in dashboard["annotations"]["list"]:
    print(f"annotation {a['name']}: {check(a['name'], a['expr'])}")
for v in dashboard["templating"]["list"]:
    if v["type"] != "query":
        continue
    metric = re.match(r"label_values\((\w+), (\w+)\)", v["query"]["query"])
    _, r = call(prom + f"/api/v1/label/{metric.group(2)}/values?" + urllib.parse.urlencode({"match[]": metric.group(1)}))
    values = r.get("data", [])
    print(f"variable {v['name']}: {values}")
    if not set(rfs) <= set(values):
        errors.append(f"variable {v['name']} lacks {sorted(set(rfs) - set(values))}")

print("--- alerts that fired")
fired, _ = query(f'max_over_time(ALERTS{{alertstate="firing"}}[{RANGE}])')
for a in fired or []:
    print(json.dumps(a["metric"]))
    errors.append(f"alert {a['metric'].get('alertname')} fired")
if not fired:
    print("none")

print("--- Grafana")
auth = "admin:soak"
status, r = call(grafana + "/api/dashboards/db", {"dashboard": dashboard, "overwrite": True}, auth)
print(f"import: {status} {r.get('status')} {r.get('url', r.get('message', ''))}")
if status != 200 or r.get("status") != "success":
    errors.append(f"Grafana didn't import the dashboard: {status} {r}")
status, r = call(grafana + "/api/dashboards/uid/" + dashboard["uid"], auth=auth)
got = r.get("dashboard", {})
print(f"loaded: {got.get('title')!r} with {len(got.get('panels', []))} panels, version {got.get('version')}")
if status != 200 or len(got.get("panels", [])) != len(dashboard["panels"]):
    errors.append(f"Grafana returned the dashboard as {status} with {len(got.get('panels', []))} panels")
failed = 0
for title, expr in targets:
    body = {"from": f"now-{seconds}s", "to": "now", "queries": [{
        "refId": "A", "datasource": {"type": "prometheus", "uid": "prometheus"},
        "expr": expr.replace("$rf", rfs[0]), "range": True, "intervalMs": 15000, "maxDataPoints": 200}]}
    status, r = call(grafana + "/api/ds/query", body, auth)
    res = r.get("results", {}).get("A", {})
    if status != 200 or res.get("error"):
        failed += 1
        errors.append(f"Grafana query of {title!r} failed: {status} {res.get('error', r)}")
print(f"{len(targets)} panel queries through Grafana's data source, {failed} failed")

if errors:
    print("--- errors")
    for e in errors:
        print("ERROR: " + e)
    sys.exit(1)
print("OK: every rule, panel, variable and annotation expression evaluates, with data where its series have it")
