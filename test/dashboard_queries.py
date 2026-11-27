#!/usr/bin/env python3
"""Run every SQL query found in Grafana dashboard JSON files against ClickHouse.

Handles classic dashboards (panels[].targets[].rawSql, templating.list[].query) and v2
resources (spec.elements.*...rawSql, spec.variables[].spec.query.spec.__legacyStringValue).
Grafana / grafana-clickhouse-datasource macros are substituted, and each query runs twice:
once with every $__conditionalAll filter disabled (the "All" case) and once with the filters
enabled and the variables set to sample values. Exits non-zero if any query fails.

Standard library only, so it runs on a bare CI runner.
"""

import argparse
import base64
import json
import re
import sys
import urllib.error
import urllib.request

# Sample values for dashboard variables when their filters are enabled.
SAMPLE_VARS = {
    "sampler": "::ffff:192.0.2.1",
    "interface": "::ffff:192.0.2.1-1",
    "showHostnames": "true",
    "srcIP": "192.0.2.10",
    "dstIP": "198.51.100.10",
    "hostIP": "192.0.2.10",
    "nextHop": "192.0.2.254",
    "srcNet": "192.0.2.0/24",
    "dstNet": "2001:db8::/32",
    "hostNet": "192.0.2.0/24",
    "srcAS": "64500",
    "dstAS": "64501",
    "extra": "Proto = 6",
    "clickhouse_adhoc_query": "default.flows_raw",
    "datasource": "b315efc1-b699-4ba8-a3a2-d6c0e711c5fd",
}
FROM_TS, TO_TS = "(toUInt64(now()) - 3600)", "toUInt64(now())"


def collect(obj, out, path=""):
    """Collects (location, sql) pairs from a dashboard JSON tree."""
    if isinstance(obj, dict):
        for k, v in obj.items():
            p = f"{path}.{k}"
            if k in ("rawSql", "__legacyStringValue") and isinstance(v, str) and v.strip():
                out.append((p, v))
            elif k == "query" and isinstance(v, str) and re.search(r"\bSELECT\b", v, re.I):
                out.append((p, v))
            else:
                collect(v, out, p)
    elif isinstance(obj, list):
        for i, v in enumerate(obj):
            collect(v, out, f"{path}[{i}]")


def split_macro_args(s, start):
    """Given s[start] == '(', returns (args, end) with end just past the closing paren."""
    depth, args, cur, i, quote = 0, [], [], start, None
    while i < len(s):
        c = s[i]
        if quote:
            cur.append(c)
            if c == "\\" and i + 1 < len(s):
                cur.append(s[i + 1])
                i += 1
            elif c == quote:
                quote = None
        elif c in "'\"":
            quote = c
            cur.append(c)
        elif c == "(":
            depth += 1
            if depth > 1:
                cur.append(c)
        elif c == ")":
            depth -= 1
            if depth == 0:
                args.append("".join(cur).strip())
                return args, i + 1
            cur.append(c)
        elif c == "," and depth == 1:
            args.append("".join(cur).strip())
            cur = []
        else:
            cur.append(c)
        i += 1
    raise ValueError("unbalanced parentheses in macro")


def replace_macro(sql, name, fn):
    out, i = [], 0
    token = "$" + name + "("
    while True:
        j = sql.find(token, i)
        if j < 0:
            out.append(sql[i:])
            return "".join(out)
        args, end = split_macro_args(sql, j + len(token) - 1)
        out.append(sql[i:j])
        out.append(fn(args))
        i = end


def substitute(sql, filters):
    sql = replace_macro(
        sql, "__conditionalAll", lambda a: f"({a[0]})" if filters else "1"
    )
    sql = replace_macro(
        sql, "__timeFilter", lambda a: f"{a[0]} BETWEEN {FROM_TS} AND {TO_TS}"
    )
    sql = replace_macro(
        sql, "__timeFilter_ms", lambda a: f"{a[0]} BETWEEN {FROM_TS} * 1000 AND {TO_TS} * 1000"
    )
    sql = replace_macro(
        sql, "__dateFilter", lambda a: f"{a[0]} BETWEEN toDate(now() - 3600) AND toDate(now())"
    )
    sql = replace_macro(
        sql, "__timeInterval", lambda a: f"toStartOfMinute(toDateTime({a[0]}))"
    )
    sql = replace_macro(
        sql, "__timeInterval_ms", lambda a: f"toStartOfMinute(toDateTime64({a[0]}, 3))"
    )
    simple = {
        "__interval_s": "60",
        "__interval_ms": "60000",
        "__interval": "1m",
        "__fromTime": f"toDateTime({FROM_TS})",
        "__toTime": f"toDateTime({TO_TS})",
        "__fromTime_ms": f"toDateTime64({FROM_TS}, 3)",
        "__toTime_ms": f"toDateTime64({TO_TS}, 3)",
        "__from": f"({FROM_TS} * 1000)",
        "__to": f"({TO_TS} * 1000)",
        "__table": "flows_raw",
    }
    for k in sorted(simple, key=len, reverse=True):
        sql = sql.replace("${" + k + "}", simple[k]).replace("$" + k, simple[k])
    values = dict(SAMPLE_VARS)
    if not filters:
        values.update(sampler="", interface="")

    def var(m):
        name = m.group(1) or m.group(2)
        if name not in values:
            raise KeyError(f"no sample value for ${name}")
        return values[name]

    sql = re.sub(r"\$\{(\w+)(?::\w+)?\}|\$([A-Za-z]\w*)", var, sql)
    if "$" in sql:
        raise KeyError("unsubstituted macro left: " + sql[sql.index("$"):][:40])
    return sql


def run(url, user, password, sql):
    auth = base64.b64encode(f"{user}:{password}".encode()).decode()
    wrapped = "SELECT count() FROM (\n" + sql.strip().rstrip(";") + "\n)"
    req = urllib.request.Request(
        url + "?max_execution_time=60",
        data=wrapped.encode(),
        headers={"Authorization": "Basic " + auth},
    )
    with urllib.request.urlopen(req, timeout=90) as r:
        return r.read().decode().strip()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--user", required=True)
    ap.add_argument("--password", required=True)
    ap.add_argument("files", nargs="+")
    a = ap.parse_args()

    total, failures = 0, 0
    for f in a.files:
        with open(f) as fh:
            dash = json.load(fh)
        title = dash.get("title") or dash.get("spec", {}).get("title") or f
        queries = []
        collect(dash, queries)
        seen = set()
        ok = 0
        for loc, sql in queries:
            if sql in seen:
                continue
            seen.add(sql)
            for filters in (False, True):
                total += 1
                try:
                    run(a.url, a.user, a.password, substitute(sql, filters))
                    ok += 1
                except urllib.error.HTTPError as e:
                    failures += 1
                    msg = e.read().decode(errors="replace").strip().splitlines()[0][:400]
                    print(f"FAIL {title} {loc} (filters={filters}): {msg}")
                except Exception as e:  # noqa: BLE001
                    failures += 1
                    print(f"FAIL {title} {loc} (filters={filters}): {e}")
        print(f"ok   {title}: {ok}/{2 * len(seen)} query runs ({len(seen)} distinct queries)")
    if failures:
        print(f"{failures}/{total} dashboard query runs failed")
        sys.exit(1)


if __name__ == "__main__":
    main()
