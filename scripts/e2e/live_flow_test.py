#!/usr/bin/env python3
"""
live_flow_test.py: drive the payroll API end to end, the way it is used live.

Every persona talks to the running HTTP API exactly as a real client would:
its own client IP (X-Forwarded-For through a trusted proxy), its own token,
Idempotency-Key headers where the API requires them. Nothing reaches into the
database except the operator persona, because provisioning an employer has no
API (see the report).

Personas
  ops          health, readiness, metrics, scheduled background jobs
  operator     provisions employer organisations
  employer     Ada, HR admin at Swift Logistics (policy, funding, staff,
               timesheets, grants, payroll, analytics, termination)
  viewer       Tunde, a view-only login
  compliance   Kemi, a compliance-role login
  worker       Musa (salaried), Chioma (hourly), Hauwa and Ibrahim (Kano Textiles)
  d2c          Bayo, self-employed, signs up directly
  monnify      the payment provider, sending HMAC-signed callbacks
  attacker     tries to get in, cross tenants, forge callbacks, brute force

Each check records what a correct product should answer and what came back.
Observations record behaviour that works as coded but is a product gap.
The run ends with a per-persona summary, endpoint coverage, latency, and
report.md / report.json in --out. Exit status is 0 only if every check passed.

In MOCK_MODE, OTP codes are written to the API log ("MOCK OTP for <phone>:
<code>") instead of being sent by SMS; --api-log is how this script reads the
"text message".
"""

import argparse
import base64
import hashlib
import hmac
import json
import os
import random
import re
import subprocess
import sys
import time
import uuid
from datetime import date, datetime, timedelta, timezone

import requests

# Every route registered in internal/api/routes.go (gin route templates).
ROUTES = [
    ("GET", "/healthz"),
    ("GET", "/readyz"),
    ("GET", "/metrics"),
    ("POST", "/api/v1/auth/login"),
    ("POST", "/api/v1/auth/refresh"),
    ("POST", "/api/v1/worker/auth/otp"),
    ("POST", "/api/v1/worker/auth/login"),
    ("POST", "/api/v1/webhooks/monnify"),
    ("POST", "/api/v1/webhooks/d2c-debit-collection"),
    ("POST", "/api/v1/d2c/signup"),
    ("POST", "/api/v1/employees/"),
    ("GET", "/api/v1/employees/"),
    ("POST", "/api/v1/employees/:id/terminate"),
    ("POST", "/api/v1/employees/:id/hardship-grants"),
    ("GET", "/api/v1/employees/:id/hardship-grants"),
    ("POST", "/api/v1/payrolls/"),
    ("GET", "/api/v1/payrolls/:id"),
    ("GET", "/api/v1/analytics/predictive"),
    ("GET", "/api/v1/analytics/workforce-dependency"),
    ("POST", "/api/v1/consent/"),
    ("GET", "/api/v1/consent/:employee_id"),
    ("GET", "/api/v1/compliance/report"),
    ("GET", "/api/v1/time-entries/"),
    ("POST", "/api/v1/time-entries/:id/approve"),
    ("POST", "/api/v1/time-entries/:id/reject"),
    ("GET", "/api/v1/funding-account/"),
    ("POST", "/api/v1/funding-account/"),
    ("GET", "/api/v1/policy/"),
    ("PUT", "/api/v1/policy/"),
    ("GET", "/api/v1/payroll-policy/"),
    ("PUT", "/api/v1/payroll-policy/"),
    ("GET", "/api/v1/worker/wages"),
    ("POST", "/api/v1/worker/advances"),
    ("GET", "/api/v1/worker/advances"),
    ("POST", "/api/v1/worker/protected-payday"),
    ("GET", "/api/v1/worker/savings"),
    ("POST", "/api/v1/worker/savings"),
    ("GET", "/api/v1/worker/bills"),
    ("POST", "/api/v1/worker/bills"),
    ("DELETE", "/api/v1/worker/bills/:id"),
    ("GET", "/api/v1/worker/bill-timing"),
    ("POST", "/api/v1/worker/time-entries"),
    ("GET", "/api/v1/worker/time-entries"),
    ("POST", "/api/v1/worker/d2c/bank-link/initiate"),
    ("POST", "/api/v1/worker/d2c/bank-link/complete"),
    ("POST", "/api/v1/worker/d2c/bank-link/authorize-debit"),
    ("POST", "/api/v1/worker/d2c/bank-link/revoke"),
    ("POST", "/api/v1/worker/d2c/bank-link/revoke-debit-mandate"),
]

TTY = sys.stdout.isatty()


def paint(code, s):
    return f"\033[{code}m{s}\033[0m" if TTY else s


def ngn(kobo):
    return "₦{:,.2f}".format((kobo or 0) / 100)


def naira_wire(kobo):
    """Monnify sends decimal Naira on the wire, e.g. "1500.50"."""
    return f"{kobo // 100}.{kobo % 100:02d}"


def short(text, n=240):
    text = (text or "").replace("\n", " ")
    return text if len(text) <= n else text[: n - 1] + "…"


def b64url(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def forge_jwt(claims, secret, alg="HS256"):
    header = {"alg": alg, "typ": "JWT"}
    signing_input = b64url(json.dumps(header).encode()) + "." + b64url(json.dumps(claims).encode())
    if alg == "none":
        return signing_input + "."
    digest = {"HS256": hashlib.sha256, "HS512": hashlib.sha512}[alg]
    sig = hmac.new(secret.encode(), signing_input.encode(), digest).digest()
    return signing_input + "." + b64url(sig)


def sql_literal(s):
    return "'" + str(s).replace("'", "''") + "'"


def rand_phone():
    # Nigerian mobile in E.164: +234 80x/81x/70x/90x + 8 digits.
    return "+234" + random.choice(["80", "81", "70", "90"]) + f"{random.randint(0, 99_999_999):08d}"


def rand_nuban():
    return f"{random.randint(0, 9_999_999_999):010d}"


class PhaseAbort(Exception):
    """A step whose result later steps depend on did not succeed."""


class Report:
    def __init__(self):
        self.checks = []
        self.observations = []
        self.coverage = {}
        self.latencies = []
        self.phase = ""
        self.aborted = []
        self.rate_limit_retries = 0

    def start_phase(self, title):
        self.phase = title
        print()
        print(paint("1;36", f"━━ {title} " + "━" * max(0, 74 - len(title))))

    def check(self, persona, name, ok, detail="", kind="bug"):
        """kind="gap" marks a user story the current design cannot satisfy: it is
        reported, but it is a product decision rather than a defect, so it does
        not fail the run."""
        self.checks.append({"phase": self.phase, "persona": persona, "name": name, "ok": bool(ok),
                            "detail": detail, "kind": kind})
        if ok:
            mark = paint("32", "PASS")
        else:
            mark = paint("33", "GAP ") if kind == "gap" else paint("1;31", "FAIL")
        print(f"  {mark} [{persona}] {name}")
        if not ok and detail:
            print(paint("2", f"       {detail}"))
        return bool(ok)

    def observe(self, persona, title, detail, severity="medium"):
        self.observations.append(
            {"phase": self.phase, "persona": persona, "title": title, "detail": detail, "severity": severity}
        )
        print(f"  {paint('33', 'NOTE')} [{persona}] {title}")

    def cover(self, method, route, status, ms):
        self.coverage.setdefault((method, route), set()).add(status)
        self.latencies.append((method, route, ms))


class Ctx:
    def __init__(self, args):
        self.args = args
        self.base = args.base_url.rstrip("/")
        self.report = Report()
        self.monnify_secret = os.environ.get("MONNIFY_SECRET_KEY", "")
        self.jwt_secret = os.environ.get("JWT_SECRET", "")
        self.metrics_token = os.environ.get("METRICS_TOKEN", "")
        self.state = {}

    def actor(self, persona, name, ip):
        return Actor(self, persona, name, ip)

    # --- operator access (provisioning only) ---------------------------------
    def sql(self, query):
        out = subprocess.run(
            ["psql", self.args.operator_dsn, "-qAt", "-v", "ON_ERROR_STOP=1", "-c", query],
            capture_output=True, text=True, timeout=60,
        )
        if out.returncode != 0:
            raise RuntimeError(f"operator SQL failed: {out.stderr.strip()}")
        return out.stdout.strip()

    # --- the "SMS inbox" -------------------------------------------------------
    def log_offset(self):
        try:
            return os.path.getsize(self.args.api_log)
        except OSError:
            return 0

    def read_otp(self, phone, offset, timeout=5.0):
        pattern = re.compile(r"MOCK OTP for " + re.escape(phone) + r": (\d{6})")
        deadline = time.time() + timeout
        while time.time() < deadline:
            with open(self.args.api_log, "r", errors="replace") as f:
                f.seek(offset)
                found = pattern.findall(f.read())
            if found:
                return found[-1]
            time.sleep(0.2)
        return None


    def read_sms(self, phone, offset, timeout=6.0):
        """The text the (mock) SMS gateway was asked to send to this phone."""
        pattern = re.compile(r"MOCK SMS to " + re.escape(phone) + r": (.+)")
        deadline = time.time() + timeout
        while time.time() < deadline:
            with open(self.args.api_log, "r", errors="replace") as f:
                f.seek(offset)
                found = pattern.findall(f.read())
            if found:
                return found[-1]
            time.sleep(0.2)
        return None


class Resp:
    def __init__(self, r, ms):
        self.status = r.status_code
        self.headers = r.headers
        self.text = r.text
        self.ms = ms
        try:
            self.json = r.json()
        except ValueError:
            self.json = None

    def get(self, *keys, default=None):
        cur = self.json
        for k in keys:
            if isinstance(cur, dict) and k in cur:
                cur = cur[k]
            else:
                return default
        return cur


class Actor:
    def __init__(self, ctx, persona, name, ip):
        self.ctx = ctx
        self.persona = persona
        self.name = name
        self.ip = ip
        self.http = requests.Session()
        self.token = None

    def call(self, method, path, route=None, *, body=None, raw=None, headers=None, token="self",
             expect=None, label=None, idem=None, retry_429=True):
        url = self.ctx.base + path
        hdrs = {"X-Forwarded-For": self.ip, "User-Agent": f"payroll-e2e/{self.persona}"}
        tok = self.token if token == "self" else token
        if tok:
            hdrs["Authorization"] = f"Bearer {tok}"
        if idem:
            hdrs["Idempotency-Key"] = str(uuid.uuid4()) if idem is True else idem
        if headers:
            hdrs.update(headers)
        if raw is not None:
            hdrs.setdefault("Content-Type", "application/json")
        for _ in range(6):
            t0 = time.perf_counter()
            r = self.http.request(method, url, json=body if raw is None else None, data=raw,
                                  headers=hdrs, timeout=30, allow_redirects=False)
            ms = (time.perf_counter() - t0) * 1000
            if r.status_code == 429 and retry_429:
                self.ctx.report.rate_limit_retries += 1
                time.sleep(float(r.headers.get("Retry-After", "1") or 1))
                continue
            break
        resp = Resp(r, ms)
        self.ctx.report.cover(method, route or path, resp.status, ms)
        if expect is not None:
            allowed = expect if isinstance(expect, (tuple, list, set)) else (expect,)
            ok = resp.status in allowed
            detail = f"{method} {path} → {resp.status} (expected {'/'.join(map(str, allowed))}) {short(resp.text)}"
            self.ctx.report.check(self.persona, label or f"{method} {route or path}", ok, "" if ok else detail)
        return resp

    def check(self, name, ok, detail="", kind="bug"):
        return self.ctx.report.check(self.persona, name, ok, detail, kind)

    def observe(self, title, detail, severity="medium"):
        self.ctx.report.observe(self.persona, title, detail, severity)


class Monnify(Actor):
    """Plays Monnify: signs every callback with HMAC-SHA512 over the raw body."""

    def send(self, payload, label, expect, secret=None, signature=None):
        body = json.dumps(payload).encode()
        key = self.ctx.monnify_secret if secret is None else secret
        sig = hmac.new(key.encode(), body, hashlib.sha512).hexdigest() if signature is None else signature
        return self.call("POST", "/api/v1/webhooks/monnify", raw=body, token=None,
                         headers={"monnify-signature": sig}, expect=expect, label=label)

    def disbursement(self, event, reference, amount_kobo, label, expect=200, **kw):
        return self.send({
            "eventType": event,
            "eventData": {
                "batchReference": "MNFY-BATCH-" + uuid.uuid4().hex[:10],
                "transactionReference": reference,
                "status": "SUCCESS" if event == "DISBURSEMENT_SUCCESSFUL" else "FAILED",
                "amount": naira_wire(amount_kobo),
            },
        }, label, expect, **kw)

    def funding(self, account_reference, tx_reference, amount_kobo, label, expect=200):
        return self.send({
            "eventType": "SUCCESSFUL_TRANSACTION",
            "eventData": {
                "transactionReference": tx_reference,
                "amountPaid": naira_wire(amount_kobo),
                "product": {"reference": account_reference, "type": "RESERVED_ACCOUNT"},
            },
        }, label, expect)


def wait_until(fn, timeout=15.0, interval=0.5):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = fn()
        if last:
            return last
        time.sleep(interval)
    return last


def require(ok, why):
    if not ok:
        raise PhaseAbort(why)


# ════════════════════════════════════════════════════════════════════════════
# Shared helpers for flows
# ════════════════════════════════════════════════════════════════════════════

def employer_login(actor, org_id, password):
    r = actor.call("POST", "/api/v1/auth/login", body={"org_id": org_id, "password": password},
                   token=None, expect=200, label="logs in with org ID and password")
    require(r.status == 200 and r.get("token"), f"{actor.name} could not log in")
    actor.token = r.get("token")
    return r


def employer_login_email(actor, email, password, expect=200):
    r = actor.call("POST", "/api/v1/auth/login", body={"email": email, "password": password},
                   token=None, expect=expect, label="logs in with their own email and password")
    if expect == 200:
        require(r.status == 200 and r.get("token"), f"{actor.name} could not log in")
        actor.token = r.get("token")
    return r


def onboard_org_via_cli(ctx, name, admin_email):
    """The supported way to create an employer: the create-org operator mode."""
    require(ctx.args.binary, "--binary is required to onboard employers")
    env = {**os.environ, "APP_MODE": "create-org", "ORG_NAME": name, "ADMIN_EMAIL": admin_email}
    p = subprocess.run([ctx.args.binary], env=env, capture_output=True, text=True, timeout=120)
    out = p.stdout + p.stderr
    org = re.search(r"created organization (ORG-[0-9a-f]+)", out)
    pw = re.search(r"temporary password \(shown once\): (\S+)", out)
    ctx.report.check("operator", f"create-org onboards {name} and prints a one-time password",
                     p.returncode == 0 and bool(org) and bool(pw), short(out, 300))
    require(org and pw, f"create-org failed for {name}")
    return org.group(1), pw.group(1)


def first_login_and_change(actor, email, temp, new_password):
    """Temporary password -> locked session -> own password."""
    r = employer_login_email(actor, email, temp)
    actor.check("a temporary-password login is flagged must_change_password",
                r.get("must_change_password") is True, short(r.text))
    actor.call("GET", "/api/v1/employees/", expect=403,
               label="…and can do nothing until the password is changed")
    r = actor.call("POST", "/api/v1/auth/password", expect=200, label="sets their own password",
                   body={"current_password": temp, "new_password": new_password})
    require(r.status == 200 and r.get("token"), f"{actor.name} could not change password")
    actor.token = r.get("token")


def worker_login(ctx, actor, phone, wrong_code_first=False):
    off = ctx.log_offset()
    actor.call("POST", "/api/v1/worker/auth/otp", body={"phone": phone}, token=None,
               expect=202, label="asks for a login code by SMS")
    code = ctx.read_otp(phone, off)
    actor.check("receives a 6-digit code by SMS", bool(code), "no 'MOCK OTP' line appeared in the API log")
    require(code, f"no OTP delivered to {actor.name}")
    if wrong_code_first:
        wrong = "000000" if code != "000000" else "111111"
        actor.call("POST", "/api/v1/worker/auth/login", body={"phone": phone, "otp": wrong}, token=None,
                   expect=401, label="mistyped code is refused")
    r = actor.call("POST", "/api/v1/worker/auth/login", body={"phone": phone, "otp": code}, token=None,
                   expect=200, label="logs in with the code")
    require(r.status == 200 and r.get("token"), f"{actor.name} could not log in")
    actor.token = r.get("token")
    return code


def provision_employer(ctx, name, password, role="admin"):
    org_id = "ORG-" + uuid.uuid4().hex[:8]
    ctx.sql(
        "INSERT INTO organizations (id, name, password_hash, role, created_at, updated_at) VALUES ("
        f"{sql_literal(org_id)}, {sql_literal(name)}, crypt({sql_literal(password)}, gen_salt('bf', 12)), "
        f"{sql_literal(role)}, now(), now())"
    )
    return org_id


def employee_body(name, *, salary=None, hourly=None, phone=None, account=None, email=None, bvn="22233344455"):
    body = {
        "name": name,
        "email": email or f"{name.split()[0].lower()}.{uuid.uuid4().hex[:6]}@example.ng",
        "account_number": account or rand_nuban(),
        "bank_code": random.choice(["058", "011", "044", "057", "033"]),
        "bvn": bvn,
    }
    if hourly is not None:
        body.update({"wage_type": "hourly", "hourly_rate_kobo": hourly})
    else:
        body.update({"wage_type": "salaried", "salary": salary})
    if phone:
        body["phone"] = phone
    return body


def probe_missing(actor, method, path, capability, why, severity="medium", body=None):
    """What a real user would try next, for a capability the API may not have.
    A 404/405 (or a trailing-slash redirect to nowhere) means it does not exist."""
    r = actor.call(method, path, route=f"(probe) {method} {path.split('?')[0]}", body=body)
    if r.status in (301, 307, 404, 405):
        actor.observe(f"Missing: {capability}", f"{method} {path} → {r.status}. {why}", severity)
    return r


def find(items, **match):
    for it in items or []:
        if all(it.get(k) == v for k, v in match.items()):
            return it
    return None


# ════════════════════════════════════════════════════════════════════════════
# Phases
# ════════════════════════════════════════════════════════════════════════════

def phase_ops_probes(ctx):
    ops = ctx.actor("ops", "Ops on-call", "10.0.0.2")
    ops.call("GET", "/healthz", token=None, expect=200, label="liveness probe answers")
    r = ops.call("GET", "/readyz", token=None, expect=200, label="readiness probe: Postgres, Redis, encryption all OK")
    if r.json:
        ops.check("readiness reports every dependency as ok",
                  all(v == "ok" for v in (r.get("checks") or {}).values()), short(r.text))
    ops.call("GET", "/metrics", token=None, expect=401, label="/metrics refuses an unauthenticated caller")
    ops.call("GET", "/metrics", token=None, headers={"Authorization": "Bearer not-the-token"}, expect=401,
             label="/metrics refuses a wrong scrape token")
    ops.call("GET", "/metrics", token=None, headers={"Authorization": "Bearer " + ctx.metrics_token}, expect=200,
             label="Prometheus can scrape /metrics with its token")


def phase_operator(ctx):
    S = ctx.state
    S["swift_admin_email"] = f"ada-{uuid.uuid4().hex[:6]}@swift.example"
    S["swift_pw"] = "Swift-pass-" + uuid.uuid4().hex[:10]
    S["kano_pw"] = "Kano-" + uuid.uuid4().hex[:10]
    S["swift"], S["swift_temp"] = onboard_org_via_cli(ctx, "Swift Logistics Ltd", S["swift_admin_email"])
    # Kano is provisioned the legacy way (shared org password) on purpose, so
    # the backward-compatible login stays covered.
    S["kano"] = provision_employer(ctx, "Kano Textiles", S["kano_pw"], "admin")
    ctx.report.check("operator", "onboards two employer orgs (one via create-org, one legacy)", True)


def phase_employer_setup(ctx, monnify):
    S = ctx.state
    ada = ctx.actor("employer", "Ada (HR admin, Swift)", "102.89.10.11")
    S["ada"] = ada

    ada.call("POST", "/api/v1/auth/login", body={"email": S["swift_admin_email"], "password": "wrong-password"},
             token=None, expect=401, label="wrong password is refused")
    ada.call("POST", "/api/v1/auth/login", body={"email": "nobody@swift.example", "password": "wrong-password"},
             token=None, expect=401, label="unknown email is refused the same way")
    ada.call("POST", "/api/v1/auth/login", body={"org_id": S["swift"], "password": S["swift_temp"]},
             token=None, expect=401, label="an onboarded org has no shared password to log in with")
    first_login_and_change(ada, S["swift_admin_email"], S["swift_temp"], S["swift_pw"])
    r = ada.call("POST", "/api/v1/auth/refresh", expect=200, label="session renews while working")
    if r.get("token"):
        ada.token = r.get("token")

    r = ada.call("GET", "/api/v1/policy/", expect=200, label="reads the default advance policy")
    ada.check("default policy: advances enabled, 30% of earned pay",
              r.get("enabled") is True and r.get("max_accrual_pct") == 30, short(r.text))
    ada.call("PUT", "/api/v1/policy/", body={"max_accrual_pct": 150}, expect=400,
             label="an impossible policy (150% of earned pay) is refused")
    r = ada.call("PUT", "/api/v1/policy/", expect=200, label="sets Swift's advance policy", body={
        "max_accrual_pct": 50, "max_draws_per_period": 3, "require_funding_coverage": True,
        "counselling_resource_name": "Swift Employee Assistance", "counselling_contact": "+2348000000000"})
    ada.check("policy change is saved", r.get("max_accrual_pct") == 50 and r.get("require_funding_coverage") is True,
              short(r.text))

    ada.call("GET", "/api/v1/payroll-policy/", expect=200, label="reads the payroll (shift premium) policy")
    ada.call("PUT", "/api/v1/payroll-policy/", expect=200, label="sets night 1.25×, weekend 1.5×, holiday 2×",
             body={"night_shift_multiplier_bps": 12500, "weekend_shift_multiplier_bps": 15000,
                   "holiday_shift_multiplier_bps": 20000})
    ada.call("PUT", "/api/v1/payroll-policy/", body={"night_shift_multiplier_bps": -5}, expect=400,
             label="a negative shift premium is refused")

    r = ada.call("POST", "/api/v1/funding-account/", body={"contact_email": "finance@swiftlogistics.ng"},
                 expect=200, label="opens the advance funding account")
    acct = r.get("account_reference")
    require(acct, "no funding account reference")
    r2 = ada.call("POST", "/api/v1/funding-account/", body={"contact_email": "finance@swiftlogistics.ng"},
                  expect=200, label="opening it again returns the same account")
    ada.check("funding account is not duplicated", r2.get("account_reference") == acct, short(r2.text))
    ada.call("GET", "/api/v1/funding-account/", expect=200, label="checks funding status")

    tx = "MNFY-TOPUP-" + uuid.uuid4().hex[:10]
    monnify.funding(acct, tx, 2_000_000_00, "credits Swift's ₦2,000,000 top-up")
    monnify.funding(acct, tx, 2_000_000_00, "re-delivers the same top-up (must not double-count)")
    r = ada.call("GET", "/api/v1/funding-account/", expect=200, label="sees the top-up")
    exposure = r.get("exposure", "minor")
    ada.check("pool shows ₦2,000,000 available exactly once", exposure == -2_000_000_00,
              f"exposure.minor={exposure} (negative = headroom); {short(r.text)}")
    S["funding_ref"] = acct


def phase_staff(ctx):
    S = ctx.state
    ada = S["ada"]
    S["musa_phone"], S["chioma_phone"] = rand_phone(), rand_phone()
    musa = employee_body("Musa Abdullahi", salary=300_000_00, phone=S["musa_phone"], account="0123456789")
    chioma = employee_body("Chioma Okafor", hourly=1_500_00, phone=S["chioma_phone"], account="0234567891")
    emeka = employee_body("Emeka Nwosu", salary=180_000_00, account="0345678912")

    ada.call("POST", "/api/v1/employees/", body=musa, expect=400,
             label="adding staff without an Idempotency-Key is refused")
    key = str(uuid.uuid4())
    r = ada.call("POST", "/api/v1/employees/", body=musa, idem=key, expect=201,
                 label="adds Musa (salaried ₦300,000, with phone for the app)")
    require(r.status == 201, "could not create Musa")
    S["musa_id"] = r.get("id")
    S["musa_body"], S["musa_key"] = musa, key
    r2 = ada.call("POST", "/api/v1/employees/", body=musa, idem=key, expect=201,
                  label="double-clicked Save replays the first response")
    ada.check("double-click does not create a second Musa",
              r2.get("id") == S["musa_id"] and r2.headers.get("Idempotent-Replayed") == "true", short(r2.text))
    ada.call("POST", "/api/v1/employees/", body={**musa, "name": "Somebody Else"}, idem=key, expect=422,
             label="same Idempotency-Key with a different body is refused")

    r = ada.call("POST", "/api/v1/employees/", body=chioma, idem=True, expect=201,
                 label="adds Chioma (hourly ₦1,500/h, with phone)")
    require(r.status == 201, "could not create Chioma")
    S["chioma_id"] = r.get("id")
    r = ada.call("POST", "/api/v1/employees/", body=emeka, idem=True, expect=201,
                 label="adds Emeka (salaried ₦180,000, payroll only, no app login)")
    require(r.status == 201, "could not create Emeka")
    S["emeka_id"] = r.get("id")

    bad = [
        ("a 9-digit account number", {**employee_body("Bad Nuban", salary=100_000_00), "account_number": "012345678"}, 400),
        ("a local-format phone (0803…)", {**employee_body("Bad Phone", salary=100_000_00), "phone": "08031234567"}, 400),
        ("a missing BVN", {**employee_body("No Bvn", salary=100_000_00), "bvn": ""}, 400),
        ("a salaried hire with no salary", {**employee_body("No Salary", salary=0)}, 400),
        ("a duplicate email", {**employee_body("Dup Email", salary=100_000_00), "email": musa["email"]}, 409),
        ("a phone already in use", {**employee_body("Dup Phone", salary=100_000_00), "phone": S["musa_phone"]}, 409),
    ]
    for what, body, status in bad:
        ada.call("POST", "/api/v1/employees/", body=body, idem=True, expect=status, label=f"refuses {what}")

    r = ada.call("GET", "/api/v1/employees/?page=1&page_size=50", route="/api/v1/employees/", expect=200,
                 label="lists staff")
    ada.check("staff list has exactly the 3 hires", r.get("total") == 3, short(r.text))
    ada.check("account numbers are masked in the list", "0123456789" not in r.text and "6789" in r.text,
              short(r.text))

    ada.call("POST", "/api/v1/consent/", expect=201, label="records Musa's consent to EWA terms",
             body={"employee_id": S["musa_id"], "consent_type": "ewa_terms", "granted": True})
    r = ada.call("GET", f"/api/v1/consent/{S['musa_id']}", route="/api/v1/consent/:employee_id", expect=200,
                 label="pulls Musa's consent history")
    types = {c.get("consent_type") for c in (r.json or [])} if isinstance(r.json, list) else set()
    ada.check("history has payroll consent (auto) and EWA terms", {"payroll_processing", "ewa_terms"} <= types,
              short(r.text))


def phase_salaried_worker(ctx, monnify):
    S = ctx.state
    musa = ctx.actor("worker", "Musa (salaried)", "105.112.3.4")
    S["musa"] = musa
    code = worker_login(ctx, musa, S["musa_phone"], wrong_code_first=True)
    musa.call("POST", "/api/v1/worker/auth/login", body={"phone": S["musa_phone"], "otp": code}, token=None,
              expect=401, label="the same code cannot be used twice")

    r = musa.call("GET", "/api/v1/worker/wages", expect=200, label="opens the app: sees earned pay and limits")
    available, min_draw = r.get("max_advance") or 0, r.get("minimum_draw") or 0
    musa.check("app shows a usable advance limit", available >= min_draw > 0,
               f"max_advance={available} minimum_draw={min_draw} {short(r.text)}")
    require(available >= min_draw > 0, "Musa has nothing to draw")
    amount = max(min_draw, min(available // 2, 10_000_00))
    S["musa_advance_amount"] = amount

    key = str(uuid.uuid4())
    r = musa.call("POST", "/api/v1/worker/advances", body={"amount": amount, "reason": "school fees"}, idem=key,
                  expect=202, label=f"requests an advance of {ngn(amount)}")
    require(r.status == 202, "advance not approved")
    adv_id = r.get("id")
    S["musa_advance_id"] = adv_id
    musa.check("advance is approved", r.get("status") == "approved", short(r.text))
    r2 = musa.call("POST", "/api/v1/worker/advances", body={"amount": amount, "reason": "school fees"}, idem=key,
                   expect=202, label="a retried request (flaky network) returns the same advance")
    musa.check("retry does not create a second advance", r2.get("id") == adv_id, short(r2.text))

    def submitted():
        lr = musa.call("GET", "/api/v1/worker/advances")
        a = find(lr.get("data"), id=adv_id)
        return a if a and a.get("provider_reference") else None
    adv = wait_until(submitted, 15)
    musa.check("the worker process sent the transfer to the bank", bool(adv),
               "advance never got a provider_reference")
    monnify.disbursement("DISBURSEMENT_SUCCESSFUL", adv_id, amount, "confirms Musa's advance landed")
    r = musa.call("GET", "/api/v1/worker/advances", expect=200, label="checks advance history")
    a = find(r.get("data"), id=adv_id)
    musa.check("advance shows as disbursed", a and a.get("status") == "disbursed", short(r.text))

    r = musa.call("POST", "/api/v1/worker/advances", body={"amount": min_draw}, idem=True, expect=422,
                  label="a second advance the same day is declined (24h cooling-off)")
    musa.check("decline explains why and when to come back",
               bool(r.get("decline_reason")) and "next_eligible_at" in (r.json or {}), short(r.text))
    r = musa.call("POST", "/api/v1/worker/advances", body={"amount": 5_000_000_00}, idem=True, expect=422,
                  label="asking for ₦5,000,000 is declined")

    musa.call("POST", "/api/v1/worker/protected-payday", body={"amount": 50_000_00}, expect=200,
              label="protects ₦50,000 of payday for rent")
    musa.call("POST", "/api/v1/worker/protected-payday", body={"amount": 10_000_00}, expect=422,
              label="lowering protection straight away is held back")

    musa.call("POST", "/api/v1/worker/savings", body={"enabled": True, "mode": "fixed_percent", "amount": 95},
              expect=400, label="saving 95% of pay is refused (cap is 90%)")
    musa.call("POST", "/api/v1/worker/savings", body={"enabled": True, "mode": "fixed_percent", "amount": 5},
              expect=200, label="turns on saving 5% of every pay")
    r = musa.call("GET", "/api/v1/worker/savings", expect=200, label="checks savings")
    musa.check("savings preference is on at 5%",
               r.get("preference", "enabled") is True and r.get("preference", "fixed_percent") == 5, short(r.text))

    r = musa.call("POST", "/api/v1/worker/bills", body={"name": "Rent", "amount": 50_000_00, "due_day": 1},
                  expect=201, label="adds rent (₦50,000, due on the 1st)")
    r = musa.call("POST", "/api/v1/worker/bills", body={"name": "DSTV", "amount": 9_000_00, "due_day": 15},
                  expect=201, label="adds DSTV (₦9,000, due on the 15th)")
    dstv_id = r.get("id")
    musa.call("POST", "/api/v1/worker/bills", body={"name": "Gym", "amount": 5_000_00, "due_day": 40},
              expect=400, label="a due day of 40 is refused")
    r = musa.call("GET", "/api/v1/worker/bills", expect=200, label="lists bills")
    musa.check("two bills listed", r.get("total") == 2, short(r.text))
    musa.call("GET", "/api/v1/worker/bill-timing", expect=200, label="sees which bills fall before payday")
    if dstv_id:
        musa.call("DELETE", f"/api/v1/worker/bills/{dstv_id}", route="/api/v1/worker/bills/:id", expect=204,
                  label="cancels DSTV")
        musa.call("DELETE", f"/api/v1/worker/bills/{dstv_id}", route="/api/v1/worker/bills/:id",
                  expect=(204, 404), label="deleting it again is harmless")
        r = musa.call("GET", "/api/v1/worker/bills", expect=200, label="lists bills after cancelling DSTV")
        musa.check("only Rent is left", r.get("total") == 1 and "DSTV" not in r.text, short(r.text))
    musa.call("DELETE", "/api/v1/worker/bills/BILL-nosuch", route="/api/v1/worker/bills/:id", expect=404,
              label="deleting a bill that does not exist says not found")

    musa.call("POST", "/api/v1/worker/time-entries", expect=422, label="a salaried worker cannot log hours",
              body={"work_date": date.today().isoformat(), "minutes_worked": 60})
    musa.call("GET", "/api/v1/employees/", expect=403, label="a worker token cannot open employer pages")
    musa.call("POST", "/api/v1/auth/refresh", expect=403, label="a worker token cannot use the employer refresh")


def phase_hourly_worker(ctx, monnify):
    S = ctx.state
    ada = S["ada"]
    chioma = ctx.actor("worker", "Chioma (hourly)", "105.112.7.8")
    S["chioma"] = chioma
    worker_login(ctx, chioma, S["chioma_phone"])
    today = date.today()

    r = chioma.call("POST", "/api/v1/worker/time-entries", expect=201, label="logs an 8-hour night shift today",
                    body={"work_date": today.isoformat(), "minutes_worked": 480, "shift_type": "night",
                          "note": "warehouse night shift"})
    e1 = r.get("id")
    r = chioma.call("POST", "/api/v1/worker/time-entries", expect=201, label="logs 2 hours of stocktake today",
                    body={"work_date": today.isoformat(), "minutes_worked": 120, "note": "stocktake"})
    e2 = r.get("id")
    require(e1 and e2, "time entries not created")
    chioma.call("POST", "/api/v1/worker/time-entries", expect=422, label="tomorrow's shift cannot be logged yet",
                body={"work_date": (today + timedelta(days=1)).isoformat(), "minutes_worked": 60})
    chioma.call("POST", "/api/v1/worker/time-entries", expect=422,
                label="15 more hours today (25h total) is refused",
                body={"work_date": today.isoformat(), "minutes_worked": 900})
    chioma.call("POST", "/api/v1/worker/time-entries", expect=400, label="a date like 03-10-2026 is refused",
                body={"work_date": today.strftime("%d-%m-%Y"), "minutes_worked": 60})
    r = chioma.call("GET", "/api/v1/worker/time-entries", expect=200, label="sees her timesheet")
    chioma.check("both entries are pending review",
                 r.get("total") == 2 and all(e.get("status") == "pending" for e in r.get("data") or []), short(r.text))
    r = chioma.call("GET", "/api/v1/worker/wages", expect=200, label="checks pay before approval")
    chioma.check("unapproved hours do not count yet", (r.get("earned_to_date") or 0) == 0, short(r.text))

    r = ada.call("GET", "/api/v1/time-entries/", expect=200, label="opens the timesheet review queue")
    ada.check("queue shows Chioma's two entries", {e1, e2} <= {e.get("id") for e in r.get("data") or []},
              short(r.text))
    ada.call("POST", f"/api/v1/time-entries/{e1}/approve", route="/api/v1/time-entries/:id/approve", expect=200,
             label="approves the night shift")
    ada.call("POST", f"/api/v1/time-entries/{e1}/approve", route="/api/v1/time-entries/:id/approve", expect=409,
             label="approving twice says already resolved")
    ada.call("POST", f"/api/v1/time-entries/{e2}/reject", route="/api/v1/time-entries/:id/reject", expect=400,
             label="rejecting without a reason is refused", body={})
    ada.call("POST", f"/api/v1/time-entries/{e2}/reject", route="/api/v1/time-entries/:id/reject", expect=200,
             label="rejects the stocktake hours with a reason", body={"reason": "stocktake was cancelled"})
    S["chioma_entry"] = e1

    r = chioma.call("GET", "/api/v1/worker/wages", expect=200, label="checks pay after approval")
    earned, available, min_draw = r.get("earned_to_date") or 0, r.get("max_advance") or 0, r.get("minimum_draw") or 0
    chioma.check("approved hours now count toward earned pay", earned > 0, short(r.text))
    if available >= min_draw > 0:
        r = chioma.call("POST", "/api/v1/worker/advances", body={"amount": min_draw, "reason": "transport"},
                        idem=True, expect=202, label=f"takes a {ngn(min_draw)} advance against her shift")
        adv_id = r.get("id")
        if r.status == 202 and adv_id:
            def submitted():
                a = find(chioma.call("GET", "/api/v1/worker/advances").get("data"), id=adv_id)
                return a if a and a.get("provider_reference") else None
            wait_until(submitted, 15)
            monnify.disbursement("DISBURSEMENT_SUCCESSFUL", adv_id, min_draw, "confirms Chioma's advance landed")
            S["chioma_advance"] = (adv_id, min_draw)
    else:
        chioma.observe("Hourly worker cannot draw after one approved shift",
                       f"earned {ngn(earned)}, available {ngn(available)}, minimum draw {ngn(min_draw)}", "low")


def phase_hardship_grant(ctx, monnify):
    S = ctx.state
    ada = S["ada"]
    route = "/api/v1/employees/:id/hardship-grants"
    path = f"/api/v1/employees/{S['musa_id']}/hardship-grants"
    body = {"amount": 25_000_00, "reason": "rent shortfall after flooding"}
    ada.call("POST", path, route=route, body=body, expect=400, label="grant without an Idempotency-Key is refused")
    ada.call("POST", path, route=route, body={"amount": 0, "reason": "x"}, idem=True, expect=400,
             label="a ₦0 grant is refused")
    ada.call("POST", path, route=route, body={"amount": 5_000_000_00, "reason": "too big"}, idem=True, expect=422,
             label="a grant bigger than the funding pool is refused")
    r = ada.call("POST", path, route=route, body=body, idem=True, expect=201,
                 label="gives Musa a ₦25,000 hardship grant")
    grant_id = r.get("id")
    require(r.status == 201 and grant_id, "grant not issued")
    ada.check("grant starts as pending (not yet paid)", r.get("status") == "pending", short(r.text))

    def status():
        g = find(ada.call("GET", path, route=route).get("data"), id=grant_id)
        return g if g and g.get("status") == "submitted" else None
    ada.check("the worker process sent the grant to the bank", bool(wait_until(status, 15)),
              "grant never reached 'submitted'")
    monnify.disbursement("DISBURSEMENT_SUCCESSFUL", grant_id, 25_000_00, "confirms the grant landed")
    r = ada.call("GET", path, route=route, expect=200, label="checks Musa's grant history")
    g = find(r.get("data"), id=grant_id)
    ada.check("grant shows as disbursed", g and g.get("status") == "disbursed", short(r.text))


def phase_payroll(ctx, monnify):
    S = ctx.state
    ada = S["ada"]
    period = date.today().strftime("%Y-%m")
    bad_period = f"{date.today().year}-{date.today().month}"
    if bad_period == period:  # October–December already have two digits
        bad_period = f"{date.today().year}-13"
    ada.call("POST", "/api/v1/payrolls/", body={"period": bad_period}, idem=True, expect=400,
             label=f"period '{bad_period}' is refused (must be YYYY-MM)")
    ada.call("POST", "/api/v1/payrolls/", body={"period": period}, expect=400,
             label="running payroll without an Idempotency-Key is refused")
    r = ada.call("POST", "/api/v1/payrolls/", body={"period": period}, idem=True, expect=202,
                 label=f"runs payroll for {period}")
    pid = r.get("id")
    require(r.status == 202 and pid, "payroll not accepted")
    S["payroll_id"] = pid
    ada.call("POST", "/api/v1/payrolls/", body={"period": period}, idem=True, expect=409,
             label="running the same month twice is refused")

    route = "/api/v1/payrolls/:id"

    def sent():
        p = ada.call("GET", f"/api/v1/payrolls/{pid}", route=route)
        return p if p.get("status") == "processing" else None
    p = wait_until(sent, 20)
    ada.check("worker process handed the batch to the bank", bool(p), "payroll never reached 'processing'")
    require(p, "payroll not processed")
    items = {i["employee_id"]: i for i in p.get("items") or []}
    ada.check("every payslip line is marked as sent (processing)",
              all(i.get("status") == "processing" for i in items.values()),
              "item statuses after hand-off: " + ", ".join(f"{i['employee_name']}={i['status']}" for i in items.values()))
    ada.check("one payslip line per active employee", len(items) == 3, short(json.dumps(list(items.values()))))

    musa_item, chioma_item, emeka_item = items.get(S["musa_id"]), items.get(S["chioma_id"]), items.get(S["emeka_id"])
    if musa_item:
        expected_max = 300_000_00 - S.get("musa_advance_amount", 0)
        ada.check(f"Musa's pay ({ngn(musa_item['amount'])}) has his advance deducted",
                  0 < musa_item["amount"] <= expected_max,
                  f"amount {ngn(musa_item['amount'])}, salary ₦300,000.00, advance {ngn(S.get('musa_advance_amount'))}")
    if chioma_item:
        ada.check(f"Chioma is paid for approved hours only ({ngn(chioma_item['amount'])})",
                  0 < chioma_item["amount"] < 50_000_00, short(json.dumps(chioma_item)))
    if emeka_item:
        ada.check("Emeka is paid his full ₦180,000", emeka_item["amount"] == 180_000_00,
                  short(json.dumps(emeka_item)))

    if musa_item:
        Monnify(ctx, "attacker", "Fake Monnify", "45.155.20.20").disbursement(
            "DISBURSEMENT_SUCCESSFUL", musa_item["id"], musa_item["amount"],
            "a callback signed with the wrong secret is refused", expect=401, secret="guessed-secret")
        monnify.disbursement("DISBURSEMENT_SUCCESSFUL", musa_item["id"], musa_item["amount"] + 100_000_00,
                             "a callback with the wrong amount is refused", expect=422)
        monnify.disbursement("DISBURSEMENT_SUCCESSFUL", musa_item["id"], musa_item["amount"], "Musa's pay landed")
        monnify.disbursement("DISBURSEMENT_SUCCESSFUL", musa_item["id"], musa_item["amount"],
                             "the same callback delivered twice is harmless")
    if chioma_item:
        monnify.disbursement("DISBURSEMENT_SUCCESSFUL", chioma_item["id"], chioma_item["amount"], "Chioma's pay landed")
    if emeka_item:
        monnify.disbursement("DISBURSEMENT_FAILED", emeka_item["id"], emeka_item["amount"],
                             "Emeka's bank rejects the transfer (account closed)")

    r = ada.call("GET", f"/api/v1/payrolls/{pid}", route=route, expect=200, label="checks the batch result")
    statuses = {i["employee_id"]: i["status"] for i in r.get("items") or []}
    ada.check("Musa and Chioma completed, Emeka failed",
              statuses.get(S["musa_id"]) == "completed" and statuses.get(S["chioma_id"]) == "completed"
              and statuses.get(S["emeka_id"]) == "failed", short(r.text))
    ada.check("batch is closed (no items left pending)", r.get("pending_count") == 0, short(r.text))
    ada.check("the batch reads as partially paid, not as if nobody was paid",
              r.get("summary", "outcome") == "partially_paid"
              and r.get("summary", "paid_items") == 2 and r.get("summary", "failed_items") == 1, short(r.text))

    # What Ada does next: Emeka's account was closed; he is paid through a retry.
    if emeka_item:
        fix_bank_details(ctx, ada, S["emeka_id"])
        retry_failed_payment(ada, monnify, pid, emeka_item)

    r = ada.call("GET", "/api/v1/payrolls/?page=1&page_size=10", route="/api/v1/payrolls/", expect=200,
                 label="opens the payroll history")
    ada.check("history lists this month's run, paid in full after the retry",
              r.get("total") == 1 and (r.get("data") or [{}])[0].get("id") == pid
              and (r.get("data") or [{}])[0].get("summary", {}).get("outcome") == "paid", short(r.text))

    musa = S.get("musa")
    if musa:
        check_payslips(ctx, musa, S, musa_item, chioma_item)
    if musa and S.get("musa_advance_id"):
        lr = musa.call("GET", "/api/v1/worker/advances", expect=200, label="checks his advance after payday")
        a = find(lr.get("data"), id=S["musa_advance_id"])
        musa.check("advance is settled by payroll", a and a.get("status") == "settled", short(lr.text))


def fix_bank_details(ctx, ada, emeka_id):
    """Emeka's account was closed. Ada corrects it (and is refused for bad input)."""
    path, route = f"/api/v1/employees/{emeka_id}", "/api/v1/employees/:id"
    ada.call("PATCH", path, route=route, body={}, expect=400, label="an empty update is refused")
    ada.call("PATCH", path, route=route, body={"is_active": False}, expect=400,
             label="an unrecognised field is refused, not silently ignored")
    ada.call("PATCH", path, route=route, body={"account_number": "12345"}, expect=400,
             label="a 5-digit account number is refused")
    ada.call("PATCH", path, route=route, body={"hourly_rate_kobo": 100000}, expect=400,
             label="an hourly rate on a salaried employee is refused")
    phone = rand_phone()
    ada.call("PATCH", path, route=route, body={"phone": phone}, expect=200,
             label="registers Emeka's phone so he can log in and be told about changes")
    new_account = rand_nuban()
    off = ctx.log_offset()
    r = ada.call("PATCH", path, route=route, body={"account_number": new_account, "bank_code": "033"},
                 expect=200, label="corrects Emeka's closed bank account")
    text = ctx.read_sms(phone, off)
    ada.check("Emeka is texted that his bank details changed", bool(text), "no 'MOCK SMS' line for his phone")
    if text:
        ada.check("the text names the account by its last four digits only",
                  new_account[-4:] in text and new_account not in text, text)
    ada.check("the response masks the new account number",
              r.get("account_number") == "****" + new_account[-4:] and new_account not in r.text, short(r.text))


def check_payslips(ctx, musa, S, musa_item, chioma_item):
    route = "/api/v1/worker/payslips"
    r = musa.call("GET", route, expect=200, label="opens his payslips")
    data = r.get("data") or []
    musa.check("he sees exactly one payslip (his own, this month)", r.get("total") == 1 and len(data) == 1, short(r.text))
    if not data:
        return
    p = data[0]
    musa.check("it is for this month and shows as paid, with the date it landed",
               p.get("period") == date.today().strftime("%Y-%m") and p.get("state") == "paid"
               and bool(p.get("paid_at")), short(json.dumps(p)))
    gross, adv, sav, net = p.get("gross"), p.get("advances_deducted"), p.get("savings"), p.get("net")
    musa.check("the breakdown adds up: gross − advances − savings = net",
               None not in (gross, adv, sav, net) and gross - adv - sav == net, short(json.dumps(p)))
    musa.check("gross is his ₦300,000 salary and the advance he took is what was deducted",
               gross == 300_000_00 and adv == S.get("musa_advance_amount"), short(json.dumps(p)))
    musa.check("net matches what the bank was asked to pay", musa_item and net == musa_item["amount"],
               f"payslip {net} vs payroll line {musa_item and musa_item['amount']}")
    musa.check("no internal fields reach a worker",
               not any(k in p for k in ("attempt", "resolved_by", "resolution_note", "error_message",
                                        "organization_id", "employee_id")), short(json.dumps(p)))
    chioma = S.get("chioma")
    if chioma and chioma_item:
        c = chioma.call("GET", route, expect=200, label="Chioma opens her payslips")
        cd = (c.get("data") or [{}])[0]
        chioma.check("she sees only her own line, with her own (hourly) pay",
                     c.get("total") == 1 and cd.get("net") == chioma_item["amount"], short(c.text))


def retry_failed_payment(ada, monnify, pid, emeka_item):
    route = "/api/v1/payrolls/:id/retry"
    path = f"/api/v1/payrolls/{pid}/retry"
    ada.call("POST", path, route=route, expect=400, label="retrying without an Idempotency-Key is refused")
    r = ada.call("POST", path, route=route, idem=True, expect=202, label="retries Emeka's failed payment")
    ada.check("exactly one line is retried, none skipped", r.get("retrying") == 1 and r.get("skipped") == [],
              short(r.text))

    def resent():
        p = ada.call("GET", f"/api/v1/payrolls/{pid}", route="/api/v1/payrolls/:id")
        it = find(p.get("items"), id=emeka_item["id"])
        return it if it and it.get("status") == "processing" and it.get("attempt") == 2 else None
    ada.check("the worker sent it again as a second attempt", bool(wait_until(resent, 15)),
              "Emeka's line never reached processing at attempt 2")

    ada.call("POST", path, route=route, idem=True, expect=409, label="a batch that is processing cannot be retried")
    monnify.disbursement("DISBURSEMENT_FAILED", emeka_item["id"], emeka_item["amount"],
                         "a late 'failed' from the FIRST attempt is acknowledged but ignored")
    p = ada.call("GET", f"/api/v1/payrolls/{pid}", route="/api/v1/payrolls/:id", expect=200,
                 label="checks that the retry is still in flight")
    ada.check("Emeka's retry is untouched by the stale callback",
              (find(p.get("items"), id=emeka_item["id"]) or {}).get("status") == "processing", short(p.text))

    monnify.disbursement("DISBURSEMENT_SUCCESSFUL", emeka_item["id"] + "-R2", emeka_item["amount"],
                         "Emeka's retried payment lands (second-attempt reference)")
    p = ada.call("GET", f"/api/v1/payrolls/{pid}", route="/api/v1/payrolls/:id", expect=200,
                 label="checks the batch after the retry")
    ada.check("every line is paid and the batch is closed",
              p.get("status") == "completed" and p.get("summary", "outcome") == "paid", short(p.text))
    ada.call("POST", path, route=route, idem=True, expect=409, label="a fully paid batch cannot be retried")


def manage_people(ctx, ada):
    """Ada gives finance and compliance staff their own logins, then removes one."""
    S = ctx.state
    people = {}
    for key, role, name, ip in [("tunde", "viewer", "Tunde (finance viewer)", "102.89.40.42"),
                                ("kemi", "compliance", "Kemi (compliance)", "102.89.40.41")]:
        email = f"{key}-{uuid.uuid4().hex[:6]}@swift.example"
        r = ada.call("POST", "/api/v1/users/", body={"email": email, "name": name, "role": role},
                     expect=201, label=f"adds {name} as a {role} on the same organisation")
        require(r.status == 201, "could not create user")
        temp = r.get("temporary_password")
        ada.check(f"{name} gets a one-time password", bool(temp))
        person = ctx.actor(role, name, ip)
        pw = "Own-pass-" + uuid.uuid4().hex[:10]
        first_login_and_change(person, email, temp, pw)
        S[key], S[key + "_email"], S[key + "_pw"] = person, email, pw
        people[key] = r.get("user", "id")
    ada.call("POST", "/api/v1/users/", body={"email": "not-an-email", "role": "viewer"}, expect=400,
             label="a malformed email is refused")
    ada.call("POST", "/api/v1/users/", body={"email": S["swift_admin_email"].upper(), "role": "viewer"},
             expect=409, label="a duplicate email (any case) is refused")
    r = ada.call("GET", "/api/v1/users/", expect=200, label="lists the organisation's people")
    users = r.get("users") or []
    ada.check("three people: Ada, Tunde, Kemi", len(users) == 3, short(r.text))
    ada_id = next((u["id"] for u in users if u["email"] == S["swift_admin_email"]), "")
    S["tunde"].call("GET", "/api/v1/users/", expect=403, label="a viewer cannot manage people")
    ada.call("PATCH", "/api/v1/users/" + (ada_id or "x"), body={"role": "viewer"}, expect=409,
             label="cannot demote the only admin")

    # Audit trail names the person, not the role.
    who = ctx.sql("SELECT actor_key FROM audit_events WHERE organization_id = "
                  f"{sql_literal(S['swift'])} AND action = 'user_created' AND actor_key LIKE 'EUS-%' LIMIT 1")
    ada.check("a user created by a person is attributed to that person's id (EUS-…)", who.startswith("EUS-"), who)

    # Removal takes effect immediately, even for a token still inside its 8h life.
    old_token = S["tunde"].token
    ada.call("PATCH", "/api/v1/users/" + people["tunde"], body={"is_active": False}, expect=200,
             label="deactivates Tunde")
    S["tunde"].token = old_token
    S["tunde"].call("GET", "/api/v1/employees/", expect=401, label="Tunde's live token stops working at once")
    employer_login_email(S["tunde"], S["tunde_email"], S["tunde_pw"], expect=401)
    ada.call("PATCH", "/api/v1/users/" + people["tunde"], body={"is_active": True}, expect=200,
             label="reinstates Tunde")
    r = ada.call("POST", f"/api/v1/users/{people['tunde']}/reset-password", expect=200,
                 label="resets Tunde's forgotten password")
    first_login_and_change(S["tunde"], S["tunde_email"], r.get("temporary_password"),
                           "Fresh-pass-" + uuid.uuid4().hex[:10])


def phase_reporting(ctx):
    S = ctx.state
    ada = S["ada"]
    ada.call("GET", "/api/v1/analytics/predictive", expect=200, label="views next month's cash-flow forecast")
    ada.call("GET", "/api/v1/analytics/workforce-dependency", expect=200,
             label="views how reliant staff are on advances")
    ada.call("GET", "/api/v1/compliance/report", expect=403, label="an admin login cannot open the compliance report")
    manage_people(ctx, ada)

    kemi = S["kemi"]
    r = kemi.call("GET", "/api/v1/compliance/report", expect=200, label="downloads the 30-day evidence bundle")
    kemi.check("report covers Swift's payroll", (r.get("payroll_summary", "total_batches") or 0) > 0,
               short(r.text))

    tunde = S["tunde"]
    r = tunde.call("GET", "/api/v1/employees/", expect=200, label="opens the staff list")
    tunde.check("sees Swift's staff", (r.get("total") or 0) == 3, short(r.text))
    tunde.call("PUT", "/api/v1/policy/", body={"max_accrual_pct": 100}, expect=403,
               label="cannot change the advance policy")
    tunde.call("POST", "/api/v1/employees/", body=employee_body("Ghost", salary=1_00), idem=True, expect=403,
               label="cannot add staff")
    tunde.call("POST", "/api/v1/payrolls/", body={"period": date.today().strftime("%Y-%m")}, idem=True,
               expect=403, label="cannot run payroll")


def phase_termination(ctx):
    S = ctx.state
    ada, chioma = S["ada"], S.get("chioma")
    route = "/api/v1/employees/:id/terminate"
    ada.call("POST", f"/api/v1/employees/{S['chioma_id']}/terminate", route=route, body={}, expect=400,
             label="terminating without a reason is refused")
    r = ada.call("POST", f"/api/v1/employees/{S['chioma_id']}/terminate", route=route,
                 body={"reason": "contract ended"}, expect=200, label="off-boards Chioma")
    ada.check("Chioma is now inactive", r.get("is_active") is False, short(r.text))
    ada.call("POST", f"/api/v1/employees/{S['chioma_id']}/terminate", route=route,
             body={"reason": "contract ended"}, expect=200, label="terminating twice is harmless")
    ada.call("POST", f"/api/v1/employees/EMP-nosuch/terminate", route=route, body={"reason": "x"}, expect=404,
             label="terminating an unknown ID says not found")
    ada.call("POST", f"/api/v1/employees/{S['chioma_id']}/hardship-grants",
             route="/api/v1/employees/:id/hardship-grants", body={"amount": 1_000_00, "reason": "x"}, idem=True,
             expect=404, label="a terminated employee cannot be given a grant")
    if chioma:
        chioma.call("GET", "/api/v1/worker/wages", expect=401, label="her open app session stops working at once")
        off = ctx.log_offset()
        chioma.call("POST", "/api/v1/worker/auth/otp", body={"phone": S["chioma_phone"]}, token=None, expect=202,
                    label="asking for a new code looks normal")
        chioma.check("…but no code is actually sent", ctx.read_otp(S["chioma_phone"], off, timeout=2) is None)


def phase_d2c(ctx):
    S = ctx.state
    bayo = ctx.actor("d2c", "Bayo (self-employed tailor)", "197.210.5.6")
    S["bayo_phone"] = rand_phone()
    body = {"name": "Bayo Adewale", "phone": S["bayo_phone"], "email": f"bayo.{uuid.uuid4().hex[:6]}@example.ng",
            "account_number": rand_nuban(), "bank_code": "058", "bvn": "22233344466", "accepted_disclosure": True}
    bayo.call("POST", "/api/v1/d2c/signup", body={**body, "accepted_disclosure": False}, token=None, expect=400,
              label="must accept the direct-debit disclosure")
    bayo.call("POST", "/api/v1/d2c/signup", body={**body, "account_number": "12345"}, token=None, expect=400,
              label="a 5-digit account number is refused")
    r = bayo.call("POST", "/api/v1/d2c/signup", body=body, token=None, expect=201, label="signs up")
    require(r.status == 201 and r.get("token"), "D2C signup failed")
    bayo.token = r.get("token")
    bayo.call("POST", "/api/v1/d2c/signup", body={**body, "email": f"x.{uuid.uuid4().hex[:6]}@example.ng"},
              token=None, expect=409, label="signing up twice with the same phone is refused")

    base = "/api/v1/worker/d2c/bank-link"
    bayo.call("POST", f"{base}/authorize-debit", body={"consent": True}, expect=404,
              label="cannot authorise debits before linking a bank")
    r = bayo.call("POST", f"{base}/initiate", expect=200, label="starts linking his bank")
    session = r.get("session_token")
    bayo.call("POST", f"{base}/complete", body={"callback_token": session, "consent": False}, expect=400,
              label="must consent to finish linking")
    bayo.call("POST", f"{base}/complete", body={"callback_token": session, "consent": True}, expect=201,
              label="finishes linking his bank")
    bayo.call("POST", f"{base}/authorize-debit", body={"consent": True}, expect=200,
              label="authorises repayment by direct debit")

    r = bayo.call("GET", "/api/v1/worker/wages", expect=200, label="opens the app")
    r = bayo.call("POST", "/api/v1/worker/advances", body={"amount": 5_000_00}, idem=True, expect=(202, 422),
                  label="requests a ₦5,000 advance")
    if r.status == 422 and r.get("decline_reason") == "no_income_history":
        bayo.observe("D2C advances cannot be demonstrated end to end",
                     "Bank linking works, but the mock bank provider returns no transactions, so every D2C "
                     "advance is declined 'no_income_history'. There is no way to show a D2C advance, its "
                     "repayment debit or the debit webhook succeeding until a real aggregator (Mono/Okra) is wired.",
                     "medium")
    Monnify(ctx, "d2c", "Bank aggregator", "41.58.0.9").call(
        "POST", "/api/v1/webhooks/d2c-debit-collection", token=None, expect=404,
        body={"provider_reference": "mock-debit-unknown", "status": "successful"},
        label="aggregator callback for an unknown debit is rejected")
    bayo.call("POST", f"{base}/revoke-debit-mandate", expect=200, label="withdraws the direct-debit authority")
    bayo.call("POST", f"{base}/revoke", expect=200, label="unlinks his bank")
    bayo.call("GET", "/api/v1/employees/", expect=403, label="a D2C worker cannot open employer pages")

    bayo2 = ctx.actor("d2c", "Bayo (next day, new phone session)", "197.210.5.6")
    worker_login(ctx, bayo2, S["bayo_phone"])


def stuck_payout_flow(ctx, sani):
    """Kano runs payroll and Monnify's callbacks never arrive."""
    period = date.today().strftime("%Y-%m")
    r = sani.call("POST", "/api/v1/payrolls/", body={"period": period}, idem=True, expect=202,
                  label="runs payroll; the bank never calls back")
    require(r.status == 202 and r.get("id"), "Kano payroll not accepted")
    pid = r.get("id")
    route = "/api/v1/payrolls/:id"

    def processing():
        p = sani.call("GET", f"/api/v1/payrolls/{pid}", route=route)
        return p if p.get("status") == "processing" else None
    p = wait_until(processing, 20)
    require(p, "Kano payroll never reached processing")
    items = p.get("items") or []
    item_path = lambda it: f"/api/v1/payrolls/{pid}/items/{it['id']}/resolve"  # noqa: E731
    rroute = "/api/v1/payrolls/:id/items/:item_id/resolve"

    sani.call("POST", item_path(items[0]), route=rroute, idem=True, expect=409,
              body={"outcome": "paid", "note": "I think it went through"},
              label="cannot mark a payment paid before its callback is overdue")
    sani.check("nothing is flagged as stuck yet", p.get("summary", "stuck") is False, short(p.text))

    time.sleep(ctx.args.stuck_after + 1)
    p = sani.call("GET", f"/api/v1/payrolls/{pid}", route=route, expect=200, label="checks the payroll later")
    sani.check("the overdue callbacks are flagged on the payroll", p.get("summary", "stuck") is True, short(p.text))

    sani.call("POST", item_path(items[0]), route=rroute, idem=True, expect=400, body={"outcome": "paid"},
              label="resolving without a reason is refused")
    sani.call("POST", item_path(items[0]), route=rroute, idem=True, expect=400,
              body={"outcome": "maybe", "note": "unsure"}, label="an outcome other than paid/failed is refused")
    sani.call("POST", item_path(items[0]), route=rroute, idem=True, expect=400,
              body={"outcome": "failed", "note": "it probably failed"},
              label="marking a payment failed without evidence is refused (a retry could pay twice)")
    swift_item = find(((ctx.state.get("ada").call("GET", f"/api/v1/payrolls/{ctx.state['payroll_id']}",
                                                  route=route).get("items")) or []))
    if swift_item:
        sani.call("POST", f"/api/v1/payrolls/{ctx.state['payroll_id']}/items/{swift_item['id']}/resolve",
                  route=rroute, idem=True, expect=404, body={"outcome": "paid", "note": "hostile attempt"},
                  label="cannot resolve another employer's payment")

    for n, it in enumerate(items):
        r = sani.call("POST", item_path(it), route=rroute, idem=True, expect=200,
                      body={"outcome": "paid", "note": "confirmed settled on the Monnify dashboard",
                            "evidence": f"MNFY-DASH-{n}"},
                      label=f"resolves {it['employee_name']}'s payment as paid, with evidence")
        sani.check("the attestation is recorded on the payment",
                   r.get("resolution_evidence") == f"MNFY-DASH-{n}" and r.get("status") == "completed", short(r.text))
    p = sani.call("GET", f"/api/v1/payrolls/{pid}", route=route, expect=200, label="checks the payroll after resolving")
    sani.check("resolving the last payment closes the batch as paid",
               p.get("status") == "completed" and p.get("summary", "outcome") == "paid", short(p.text))
    Monnify(ctx, "monnify", "Monnify", "41.58.0.10").disbursement(
        "DISBURSEMENT_FAILED", items[0]["id"], items[0]["amount"],
        "the callback finally turns up, contradicting the attestation, and is ignored")
    p = sani.call("GET", f"/api/v1/payrolls/{pid}", route=route, expect=200, label="re-checks the payroll")
    sani.check("a late callback cannot undo a resolved payment", p.get("status") == "completed", short(p.text))


def phase_second_tenant(ctx):
    S = ctx.state
    sani = ctx.actor("employer", "Sani (HR admin, Kano Textiles)", "41.190.2.3")
    S["sani"] = sani
    employer_login(sani, S["kano"], S["kano_pw"])
    sani.call("PUT", "/api/v1/policy/", body={"cooling_off_hours": 0}, expect=200,
              label="turns off the cooling-off period")
    S["hauwa_phone"], S["ibrahim_phone"] = rand_phone(), rand_phone()
    r = sani.call("POST", "/api/v1/employees/", idem=True, expect=201, label="adds Hauwa",
                  body=employee_body("Hauwa Bello", salary=150_000_00, phone=S["hauwa_phone"]))
    S["hauwa_id"] = r.get("id")
    sani.call("POST", "/api/v1/employees/", idem=True, expect=201, label="adds Ibrahim",
              body=employee_body("Ibrahim Musa", salary=120_000_00, phone=S["ibrahim_phone"]))

    stuck_payout_flow(ctx, sani)

    hauwa = ctx.actor("worker", "Hauwa (Kano Textiles)", "105.112.9.9")
    worker_login(ctx, hauwa, S["hauwa_phone"])
    hauwa.call("POST", "/api/v1/worker/protected-payday", body={"amount": 30_000_00}, expect=200,
               label="protects ₦30,000 of payday")
    hauwa.call("POST", "/api/v1/worker/protected-payday", body={"amount": 0}, expect=200,
               label="removes the protection (no cooling-off at this employer)")
    hauwa.call("POST", "/api/v1/worker/savings", body={"enabled": False}, expect=200,
               label="leaves savings switched off")


def phase_attacker(ctx, monnify):
    S = ctx.state
    mal = ctx.actor("attacker", "Attacker", "45.155.20.20")
    mal.call("GET", "/api/v1/employees/", token=None, expect=401, label="no token: refused")
    mal.call("GET", "/api/v1/employees/", token="not-a-jwt", expect=401, label="garbage token: refused")
    now = int(time.time())
    claims = {"org_id": S["swift"], "role": "admin", "exp": now + 3600, "iat": now, "auth_time": now}
    mal.call("GET", "/api/v1/employees/", token=forge_jwt(claims, "guessed-secret"), expect=401,
             label="token signed with a guessed secret: refused")
    mal.call("GET", "/api/v1/employees/", token=forge_jwt(claims, "", alg="none"), expect=401,
             label="unsigned (alg=none) token: refused")
    if ctx.jwt_secret:
        mal.call("GET", "/api/v1/employees/", expect=401, label="a stolen but expired token: refused",
                 token=forge_jwt({**claims, "exp": now - 60}, ctx.jwt_secret))
        mal.call("GET", "/api/v1/employees/", expect=401, label="a token with no expiry: refused",
                 token=forge_jwt({k: v for k, v in claims.items() if k != "exp"}, ctx.jwt_secret))
        mal.call("GET", "/api/v1/employees/", expect=401, label="a token signed HS512 instead of HS256: refused",
                 token=forge_jwt(claims, ctx.jwt_secret, alg="HS512"))
        mal.call("POST", "/api/v1/auth/refresh", expect=401, label="a 25-hour-old session cannot be renewed",
                 token=forge_jwt({**claims, "auth_time": now - 25 * 3600}, ctx.jwt_secret))

    sani = S["sani"]
    route_p = "/api/v1/payrolls/:id"
    if S.get("payroll_id"):
        sani.call("GET", f"/api/v1/payrolls/{S['payroll_id']}", route=route_p, expect=404,
                  label="Kano admin cannot read Swift's payroll")
    sani.call("POST", f"/api/v1/employees/{S['musa_id']}/terminate", route="/api/v1/employees/:id/terminate",
              body={"reason": "hostile"}, expect=404, label="Kano admin cannot terminate Swift's employee")
    sani.call("PATCH", f"/api/v1/employees/{S['musa_id']}", route="/api/v1/employees/:id",
              body={"account_number": rand_nuban()}, expect=404,
              label="Kano admin cannot redirect Swift's employee's salary to another account")
    sani.call("POST", f"/api/v1/employees/{S['musa_id']}/hardship-grants",
              route="/api/v1/employees/:id/hardship-grants", body={"amount": 1_000_00, "reason": "x"},
              idem=True, expect=404, label="Kano admin cannot pay a grant to Swift's employee")
    if S.get("chioma_entry"):
        sani.call("POST", f"/api/v1/time-entries/{S['chioma_entry']}/approve",
                  route="/api/v1/time-entries/:id/approve", expect=404,
                  label="Kano admin cannot approve Swift's timesheets")
    r = sani.call("GET", "/api/v1/employees/", expect=200, label="Kano admin lists staff")
    sani.check("Kano sees only its own staff", S["musa_id"] not in r.text and r.get("total") == 2, short(r.text))
    r = sani.call("GET", f"/api/v1/consent/{S['musa_id']}", route="/api/v1/consent/:employee_id", expect=200,
                  label="Kano admin asks for Swift employee's consent history")
    sani.check("…and gets nothing back", r.json == [] or r.json is None, short(r.text))
    sani.call("POST", "/api/v1/consent/", expect=(400, 404), label="Kano admin cannot record consent for Swift's employee",
              body={"employee_id": S["musa_id"], "consent_type": "marketing", "granted": False})
    sani.call("POST", "/api/v1/consent/", expect=(400, 404),
              label="recording consent for an employee ID that does not exist is refused",
              body={"employee_id": "EMP-nosuch00", "consent_type": "marketing", "granted": False})
    r = sani.call("POST", "/api/v1/employees/", body={**S["musa_body"], "email": f"k.{uuid.uuid4().hex[:6]}@example.ng",
                                                     "phone": rand_phone()},
                  idem=S["musa_key"], expect=201, label="reusing Swift's Idempotency-Key does not replay Swift's data")
    sani.check("Kano gets its own new record, not Swift's Musa",
               r.get("id") and r.get("id") != S["musa_id"] and r.headers.get("Idempotent-Replayed") != "true",
               short(r.text))

    if S.get("payroll_id"):
        Monnify(ctx, "attacker", "Fake Monnify", "45.155.20.20").call(
            "POST", "/api/v1/webhooks/monnify", token=None, expect=401, label="a callback with no signature is refused",
            body={"eventType": "DISBURSEMENT_SUCCESSFUL",
                  "eventData": {"transactionReference": "ITEM-x", "amount": "1.00"}})

    # OTP brute force against a worker who never logged in.
    brute = ctx.actor("attacker", "Attacker (OTP guessing)", "45.155.20.21")
    off = ctx.log_offset()
    brute.call("POST", "/api/v1/worker/auth/otp", body={"phone": S["ibrahim_phone"]}, token=None, expect=202,
               label="triggers a code to Ibrahim's phone")
    real = ctx.read_otp(S["ibrahim_phone"], off)
    guesses = [f"{n:06d}" for n in random.sample(range(1_000_000), 6) if f"{n:06d}" != real][:5]
    for g in guesses:
        brute.call("POST", "/api/v1/worker/auth/login", body={"phone": S["ibrahim_phone"], "otp": g}, token=None,
                   expect=401, label=f"guess {g}: refused")
    if real:
        brute.call("POST", "/api/v1/worker/auth/login", body={"phone": S["ibrahim_phone"], "otp": real}, token=None,
                   expect=401, label="after 5 wrong guesses even the real code is dead")

    # Password spraying from one IP.
    spray = ctx.actor("attacker", "Attacker (password spraying)", "45.155.20.22")
    got429, retry_after = False, None
    for i in range(15):
        r = spray.call("POST", "/api/v1/auth/login", body={"email": S["swift_admin_email"], "password": f"guess-{i}"},
                       token=None, retry_429=False)
        if r.status == 429:
            got429, retry_after = True, r.headers.get("Retry-After")
            break
    spray.check("rapid login attempts from one IP are throttled (429)", got429)
    spray.check("…with a Retry-After header", bool(retry_after))

    big = ctx.actor("attacker", "Attacker (oversized body)", "45.155.20.23")
    big.call("POST", "/api/v1/auth/login", raw=b'{"org_id":"' + b"A" * (2 << 20) + b'","password":"x"}',
             token=None, expect=(400, 413), label="a 2 MB request body is refused, not processed")
    big.call("POST", "/api/v1/auth/login", body={"org_id": "ORG' OR '1'='1", "password": "' OR 1=1 --"},
             token=None, expect=401, label="SQL-injection-looking credentials are just wrong credentials")


def phase_jobs(ctx):
    if not ctx.args.binary:
        return
    out_dir = os.path.join(ctx.args.out, "evidence")
    os.makedirs(out_dir, exist_ok=True)
    for mode, purpose in [
        ("snapshot-accruals", "daily accrual snapshot"),
        ("collect-evidence", "daily SOC 2 evidence collection"),
        ("reconcile", "hourly ledger vs wallet reconciliation"),
        ("collect-d2c-debits", "D2C repayment debit sweep"),
    ]:
        env = {**os.environ, "APP_MODE": mode, "SOC2_EVIDENCE_DIR": out_dir}
        t0 = time.time()
        p = subprocess.run([ctx.args.binary], env=env, capture_output=True, text=True, timeout=180)
        tail = short((p.stdout + p.stderr).strip().splitlines()[-1] if (p.stdout + p.stderr).strip() else "", 300)
        ctx.report.check("ops", f"scheduled job '{mode}' ({purpose}) exits cleanly", p.returncode == 0,
                         f"exit {p.returncode} after {time.time() - t0:.1f}s: {tail}")
        ctx.report.cover("JOB", mode, p.returncode, (time.time() - t0) * 1000)
    files = os.listdir(out_dir)
    ctx.report.check("ops", "SOC 2 evidence file was written", bool(files), f"no files in {out_dir}")


def phase_metrics_leak(ctx):
    ops = ctx.actor("ops", "Ops on-call", "10.0.0.2")
    anon = ops.call("GET", "/metrics", token=None, expect=401, label="an anonymous caller cannot read metrics")
    leaked = [o for o in (ctx.state.get("swift"), ctx.state.get("kano")) if o and o in anon.text]
    ops.check("an anonymous caller learns no tenant IDs from /metrics", not leaked, ", ".join(leaked))
    m = ops.call("GET", "/metrics", token=None, headers={"Authorization": "Bearer " + ctx.metrics_token},
                 expect=200, label="the scraper still sees per-tenant series after a day of traffic")
    ops.check("per-tenant series exist for the scraper", ctx.state.get("swift", "") in m.text or "org_id" in m.text)


# ════════════════════════════════════════════════════════════════════════════
# Reporting
# ════════════════════════════════════════════════════════════════════════════

def summarise(ctx):
    rep = ctx.report
    passed = sum(1 for c in rep.checks if c["ok"])
    failed = [c for c in rep.checks if not c["ok"] and c["kind"] == "bug"]
    gaps = [c for c in rep.checks if not c["ok"] and c["kind"] == "gap"]

    print()
    print(paint("1", "═" * 78))
    print(paint("1", f" RESULT: {passed} passed, {len(failed)} failed, {len(gaps)} design gaps, "
                     f"{len(rep.observations)} observations"))
    print(paint("1", "═" * 78))

    personas = {}
    for c in rep.checks:
        p = personas.setdefault(c["persona"], [0, 0, 0])
        p[0 if c["ok"] else (1 if c["kind"] == "bug" else 2)] += 1
    print("\n Per persona:")
    for name, (ok, bad, gap) in sorted(personas.items()):
        print(f"   {name:<12} {ok:>3} passed  {bad:>3} failed  {gap:>3} gaps")

    if failed:
        print(paint("1;31", "\n Failed checks:"))
        for c in failed:
            print(f"   ✗ [{c['persona']}] {c['name']}")
            if c["detail"]:
                print(paint("2", f"       {c['detail']}"))
    if gaps:
        print(paint("33", "\n Design gaps (user stories the current design cannot satisfy):"))
        for c in gaps:
            print(f"   ~ [{c['persona']}] {c['name']}")
    if rep.aborted:
        print(paint("1;31", "\n Phases cut short:"))
        for phase, why in rep.aborted:
            print(f"   • {phase}: {why}")
    if rep.observations:
        print(paint("33", "\n Observations (works as coded, but a product gap):"))
        for o in rep.observations:
            print(f"   • [{o['severity']}] {o['title']}")

    untested = [r for r in ROUTES if r not in rep.coverage]
    print(f"\n Endpoint coverage: {len(ROUTES) - len(untested)}/{len(ROUTES)} routes exercised")
    for m, r in untested:
        print(paint("31", f"   not exercised: {m} {r}"))

    lat = sorted((ms, m, r) for m, r, ms in rep.latencies if m != "JOB")
    if lat:
        ms_only = [x[0] for x in lat]
        p50 = ms_only[len(ms_only) // 2]
        p95 = ms_only[int(len(ms_only) * 0.95) - 1]
        print(f"\n Latency over {len(lat)} requests: p50 {p50:.0f} ms, p95 {p95:.0f} ms, max {lat[-1][0]:.0f} ms "
              f"({lat[-1][1]} {lat[-1][2]})")
    if rep.rate_limit_retries:
        print(f" Requests that hit the rate limit and were retried: {rep.rate_limit_retries}")

    write_reports(ctx, passed, failed, untested, lat)
    return 0 if not failed and not rep.aborted else 1


def write_reports(ctx, passed, failed, untested, lat):
    rep = ctx.report
    os.makedirs(ctx.args.out, exist_ok=True)
    data = {
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "base_url": ctx.base,
        "passed": passed,
        "failed": len(failed),
        "design_gaps": sum(1 for c in rep.checks if not c["ok"] and c["kind"] == "gap"),
        "checks": rep.checks,
        "observations": rep.observations,
        "aborted_phases": rep.aborted,
        "coverage": {f"{m} {r}": sorted(s) for (m, r), s in rep.coverage.items()},
        "untested_routes": [f"{m} {r}" for m, r in untested],
    }
    with open(os.path.join(ctx.args.out, "report.json"), "w") as f:
        json.dump(data, f, indent=2, default=str)

    lines = [f"# Live flow test — {data['generated_at']}", "",
             f"**{passed} passed, {len(failed)} failed, {data['design_gaps']} design gaps, "
             f"{len(rep.observations)} observations**", ""]
    phase = None
    for c in rep.checks:
        if c["phase"] != phase:
            phase = c["phase"]
            lines += ["", f"## {phase}", "", "| | Persona | Check |", "|---|---|---|"]
        detail = f"<br>`{c['detail']}`" if not c["ok"] and c["detail"] else ""
        mark = "✅" if c["ok"] else ("⚠️ gap" if c["kind"] == "gap" else "❌")
        lines.append(f"| {mark} | {c['persona']} | {c['name']}{detail} |")
    if rep.observations:
        lines += ["", "## Observations", ""]
        for o in rep.observations:
            lines.append(f"- **[{o['severity']}] {o['title']}** ({o['persona']}): {o['detail']}")
    lines += ["", "## Endpoint coverage", "", "| Method | Route | Statuses seen |", "|---|---|---|"]
    for (m, r) in ROUTES:
        seen = rep.coverage.get((m, r))
        lines.append(f"| {m} | `{r}` | {', '.join(map(str, sorted(seen))) if seen else '**not exercised**'} |")
    with open(os.path.join(ctx.args.out, "report.md"), "w") as f:
        f.write("\n".join(lines) + "\n")


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base-url", default="http://127.0.0.1:28080")
    ap.add_argument("--api-log", required=True, help="API process log (MOCK_MODE prints OTP codes there)")
    ap.add_argument("--operator-dsn", required=True, help="superuser DSN, used only to provision employer orgs")
    ap.add_argument("--binary", help="payroll binary, to run the scheduled jobs")
    ap.add_argument("--out", default=".", help="where report.md / report.json go")
    ap.add_argument("--seed", type=int, default=None, help="random seed for reproducible phones/accounts")
    ap.add_argument("--stuck-after", type=float, default=5.0,
                    help="seconds the API's PAYROLL_STUCK_AFTER is set to (run_live_e2e.sh sets 5s)")
    args = ap.parse_args()
    random.seed(args.seed)

    ctx = Ctx(args)
    monnify = Monnify(ctx, "monnify", "Monnify", "41.58.0.10")
    phases = [
        ("Ops: is the service up?", lambda: phase_ops_probes(ctx)),
        ("Operator: provision employers", lambda: phase_operator(ctx)),
        ("Employer admin: set up Swift Logistics", lambda: phase_employer_setup(ctx, monnify)),
        ("Employer admin: hire staff, record consent", lambda: phase_staff(ctx)),
        ("Salaried worker: Musa's first week with the app", lambda: phase_salaried_worker(ctx, monnify)),
        ("Hourly worker: Chioma logs shifts, Ada reviews them", lambda: phase_hourly_worker(ctx, monnify)),
        ("Employer admin: hardship grant for Musa", lambda: phase_hardship_grant(ctx, monnify)),
        ("Payday: payroll run and Monnify callbacks", lambda: phase_payroll(ctx, monnify)),
        ("Reporting: analytics, compliance, view-only access", lambda: phase_reporting(ctx)),
        ("Off-boarding: Chioma leaves", lambda: phase_termination(ctx)),
        ("D2C: Bayo signs up without an employer", lambda: phase_d2c(ctx)),
        ("Second employer: Kano Textiles", lambda: phase_second_tenant(ctx)),
        ("Attacker: break in, cross tenants, forge, guess", lambda: phase_attacker(ctx, monnify)),
        ("Ops: scheduled background jobs", lambda: phase_jobs(ctx)),
        ("Ops: what does /metrics give away?", lambda: phase_metrics_leak(ctx)),
    ]
    for title, fn in phases:
        ctx.report.start_phase(title)
        try:
            fn()
        except PhaseAbort as e:
            ctx.report.aborted.append((title, str(e)))
            print(paint("1;31", f"  ABORTED: {e}"))
        except (KeyError, requests.RequestException, RuntimeError) as e:
            ctx.report.aborted.append((title, f"{type(e).__name__}: {e}"))
            print(paint("1;31", f"  ABORTED: {type(e).__name__}: {e}"))
    sys.exit(summarise(ctx))


if __name__ == "__main__":
    main()
