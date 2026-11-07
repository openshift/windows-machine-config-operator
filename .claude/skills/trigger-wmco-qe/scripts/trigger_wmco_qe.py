#!/usr/bin/env python3
"""Plan, submit once, and observe existing WMCO periodics through Gangway."""

import argparse
import contextlib
import datetime as dt
import fcntl
import hashlib
import http.client
import json
import os
import re
import ssl
import stat
import sys
import tempfile
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

import yaml

API = "https://gangway-ci.apps.ci.l2s4.p1.openshiftapps.com/v1/executions"
SOURCE = "https://raw.githubusercontent.com/openshift/release"
REGISTRY = "ci-operator/step-registry"
MAX_JOBS = 25
MAX_STATE = 65536
STATES = {
    "TRIGGERED",
    "SCHEDULING",
    "PENDING",
    "SUCCESS",
    "FAILURE",
    "ABORTED",
    "ERROR",
}
TERMINAL = {"SUCCESS", "FAILURE", "ABORTED", "ERROR"}
CONNECTED = "openshift-windows-setup-wmco-konflux"
DISCONNECTED = "openshift-windows-setup-wmco-konflux-disconnected"


class SafetyError(Exception):
    pass


class EvidenceConflict(SafetyError):
    pass


class UniqueLoader(yaml.SafeLoader):
    pass


def unique_mapping(loader, node):
    result = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node)
        if not isinstance(key, str) or key in result:
            raise SafetyError(
                "YAML keys must be unique strings; merges are unsupported"
            )
        result[key] = loader.construct_object(value_node)
    return result


UniqueLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, unique_mapping
)


def read_yaml(content):
    try:
        loader = UniqueLoader(content)
        try:
            value = loader.get_single_data()
        finally:
            loader.dispose()
    except (yaml.YAMLError, RecursionError, ValueError) as error:
        raise SafetyError("Unsupported YAML document") from error
    if not isinstance(value, dict):
        raise SafetyError("Expected a YAML mapping")
    return value


def digest(value):
    return hashlib.sha256(
        json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()


def inputs(version, stream, source_sha):
    if not re.fullmatch(r"(?:4|5)\.(?:0|[1-9][0-9]?)", version):
        raise SafetyError(
            "Use a canonical OCP minor such as 4.21 or 5.0, not a WMCO version"
        )
    stream = stream.lower().removesuffix("-stream").removesuffix("stream")
    if stream not in {"z", "y"}:
        raise SafetyError("Stream must be z or y")
    if not re.fullmatch(r"[0-9a-f]{40}", source_sha):
        raise SafetyError("Source must be a reviewed full openshift/release commit SHA")
    return stream


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, new_url):
        return None


def opener():
    return urllib.request.build_opener(
        urllib.request.ProxyHandler({}),
        NoRedirect(),
        urllib.request.HTTPSHandler(context=ssl.create_default_context()),
    )


class PublicSource:
    def __init__(self, source_sha):
        inputs("4.21", "z", source_sha)
        self.sha = source_sha
        self.files = {}
        self.http = opener()

    def read(self, path):
        if path in self.files:
            return self.files[path]
        if (
            len(self.files) >= 20
            or not re.fullmatch(r"ci-operator/[a-zA-Z0-9_./-]+", path)
            or ".." in path
        ):
            raise SafetyError("Unsupported public source path or source-file limit")
        url = f"{SOURCE}/{self.sha}/{path}"
        try:
            with self.http.open(url, timeout=30) as response:
                content = response.read(12 * 1024 * 1024 + 1)
                if response.geturl() != url or len(content) > 12 * 1024 * 1024:
                    raise SafetyError("Public source redirect or oversized file")
                text = content.decode("utf-8")
        except (OSError, ValueError) as error:
            raise SafetyError(
                "Public source unavailable; check the pinned commit and supported paths"
            ) from error
        self.files[path] = text
        return text


def registry_path(name, kind):
    return f"{REGISTRY}/{name.replace('-', '/')}/{name}-{kind}.yaml"


def catalog_contract(source, workflow):
    if not isinstance(workflow, str) or not re.fullmatch(
        r"cucushift-installer-rehearse-[a-z0-9]+-(?:ipi|upi)(?:-disconnected)?-ovn-winc",
        workflow,
    ):
        raise SafetyError(
            "Unsupported WMCO workflow; review its catalog contract before adding support"
        )
    disconnected = "-disconnected-" in workflow
    producer = DISCONNECTED if disconnected else CONNECTED
    consumer = (
        "openshift-windows-install-wmco" if disconnected else "operatorhub-subscribe"
    )
    steps = read_yaml(source.read(registry_path(workflow, "workflow")))["workflow"][
        "steps"
    ]
    if not disconnected:
        expected = {
            "SUB_SOURCE": "wmco",
            "SUB_PACKAGE": "windows-machine-config-operator",
            "SUB_CHANNEL": "stable",
        }
        if any(
            steps.get("env", {}).get(key) != value for key, value in expected.items()
        ):
            raise SafetyError("Unsupported WMCO subscription configuration")
    chain = workflow + "-provision"
    if steps.get("pre", [None])[0] != {"chain": chain}:
        raise SafetyError("Unsupported WMCO provision chain")
    provision = read_yaml(source.read(registry_path(chain, "chain")))["chain"]["steps"]
    refs = [step.get("ref") for step in provision]
    if (
        refs.count(producer) != 1
        or refs.count(consumer) != 1
        or refs.index(producer) >= refs.index(consumer)
    ):
        raise SafetyError(
            "Expected configured WMCO catalog producer before installation"
        )
    source.read(
        f"{REGISTRY}/openshift/windows/{producer.removeprefix('openshift-windows-')}/{producer}-commands.sh"
    )
    return producer


def build_plan(version, stream, source_sha, source=None):
    stream = inputs(version, stream, source_sha)
    source = source or PublicSource(source_sha)
    stem = f"openshift-openshift-tests-private-release-{version}"
    config = read_yaml(
        source.read(
            f"ci-operator/config/openshift/openshift-tests-private/{stem}__amd64-nightly.yaml"
        )
    )
    periodics = read_yaml(
        source.read(
            f"ci-operator/jobs/openshift/openshift-tests-private/{stem}-periodics.yaml"
        )
    )["periodics"]
    candidate = config["releases"]["latest"]["candidate"]
    if candidate != {
        "architecture": "amd64",
        "product": "ocp",
        "stream": "nightly",
        "version": version,
    }:
        raise SafetyError("Unsupported latest OCP nightly payload configuration")
    jobs, catalogs, seen = [], set(), set()
    for test in config["tests"]:
        name = test["as"]
        if not isinstance(name, str) or name in seen:
            raise SafetyError("Duplicate or malformed test name")
        seen.add(name)
        if not re.fullmatch(
            r"[a-z0-9]+(?:-[a-z0-9]+)*-winc(?:-[a-z0-9]+)*", name
        ) or "debug" in name.split("-"):
            continue
        if stream == "z" and (
            name.split("-")[0] not in {"aws", "gcp", "azure", "vsphere"}
            or not {"ipi", "upi"}.intersection(name.split("-"))
        ):
            continue
        if not isinstance(test.get("cron"), str) or not test["cron"].strip():
            raise SafetyError("Matching WMCO job is not a configured periodic")
        steps = test["steps"]
        if set(steps) - {"cluster_profile", "env", "leases", "test", "workflow"}:
            raise SafetyError("Unsupported job-level workflow customization")
        if any(
            key.startswith("SUB_") or "IMAGE" in key or "CATALOG" in key
            for key in steps.get("env", {})
        ):
            raise SafetyError("Job-level catalog/image overrides are unsupported")
        job = f"periodic-ci-{stem}-amd64-nightly-{name}"
        matches = [entry for entry in periodics if entry["name"] == job]
        if len(matches) != 1:
            raise SafetyError("Missing or duplicate existing periodic definition")
        periodic = matches[0]
        args = periodic["spec"]["containers"][0]["args"]
        if (
            periodic.get("cron") != test["cron"]
            or periodic["labels"].get("ci-operator.openshift.io/variant")
            != "amd64-nightly"
            or [arg for arg in args if arg.startswith("--target=")]
            != [f"--target={name}"]
            or [arg for arg in args if arg.startswith("--variant=")]
            != ["--variant=amd64-nightly"]
        ):
            raise SafetyError(
                "Periodic target/variant does not match ordinary-nightly configuration"
            )
        catalogs.add(catalog_contract(source, steps["workflow"]))
        jobs.append(job)
    if not 1 <= len(jobs) <= MAX_JOBS:
        raise SafetyError(
            "Expected between 1 and 25 matching ordinary-nightly WMCO jobs"
        )
    return {
        "ocp_minor": version,
        "stream": stream,
        "source_sha": source_sha,
        "jobs": sorted(jobs),
        "catalog_refs": sorted(catalogs),
        "source_fingerprint": digest(source.files),
    }


def execution_id(value):
    if not isinstance(value, str) or not re.fullmatch(
        r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}", value
    ):
        raise SafetyError("Malformed execution ID")
    if str(uuid.UUID(value)) != value:
        raise SafetyError("Malformed execution ID")
    return value


def timestamp(value):
    if not isinstance(value, str) or not re.fullmatch(
        r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?Z", value
    ):
        raise SafetyError("Missing or malformed completion/start timestamp")
    return dt.datetime.fromisoformat(value)


def prow_url(value, job):
    if not isinstance(value, str) or not re.fullmatch(
        r"https://prow\.ci\.openshift\.org/view/gs/[a-z0-9][a-z0-9.-]*/logs/"
        + re.escape(job)
        + r"/[1-9][0-9]*",
        value,
    ):
        raise SafetyError("Missing or unsupported Prow run URL")
    return value


def validate_execution(value, job, expected_id=None):
    if (
        not isinstance(value, dict)
        or value.get("job_name") != job
        or value.get("job_type") != "PERIODIC"
    ):
        raise EvidenceConflict("Unsupported API response or foreign execution job/type")
    identifier = execution_id(value.get("id"))
    if expected_id is not None and identifier != expected_id:
        raise EvidenceConflict("Foreign execution ID")
    if value.get("job_status") not in STATES:
        raise SafetyError("Missing or unknown execution status")
    if value.get("job_url"):
        prow_url(value["job_url"], job)
    if value.get("create_time"):
        timestamp(value["create_time"])
    return identifier


class Gangway:
    def __init__(self, token):
        if not isinstance(token, str) or not re.fullmatch(
            r"[\x21-\x7e]{1,8192}", token
        ):
            raise SafetyError("Approved app.ci token input is missing or invalid")
        self.token = token
        self.http = opener()

    def request(self, method, identifier=None, job=None):
        if method == "POST" and identifier is None and isinstance(job, str):
            data = json.dumps({"job_name": job, "job_execution_type": "1"}).encode()
            url = API
        elif method == "GET" and job is None:
            url, data = f"{API}/{execution_id(identifier)}", None
        else:
            raise SafetyError("Unsupported Gangway operation")
        request = urllib.request.Request(
            url,
            data=data,
            method=method,
            headers={
                "Authorization": f"Bearer {self.token}",
                "Content-Type": "application/json",
                "Accept": "application/json",
            },
        )
        try:
            with self.http.open(request, timeout=30) as response:
                if response.geturl() != url or response.status != 200:
                    raise SafetyError(
                        "Unsupported API/proxy response; redirects are forbidden"
                    )
                content = response.read(MAX_STATE + 1)
                if len(content) > MAX_STATE:
                    raise SafetyError("Oversized API response")
                return json.loads(content, object_pairs_hook=unique_json)
        except urllib.error.HTTPError as error:
            if error.code in {401, 403}:
                raise SafetyError(
                    "Gangway authentication/authorization denied"
                ) from None
            raise SafetyError(
                f"Gangway HTTP {error.code}; unsupported endpoint/proxy or service error"
            ) from None
        except (OSError, ValueError, http.client.HTTPException, RecursionError):
            raise SafetyError(
                "Gangway transport timeout/error or malformed JSON; no POST retry"
            ) from None


def state_path(value):
    path = Path(value)
    if (
        not path.is_absolute()
        or ".." in path.parts
        or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,95}\.json", path.name)
    ):
        raise SafetyError(
            "State must be an absolute, traversal-free path to a named .json file"
        )
    directory = path.parent
    if directory.resolve(strict=True) != directory:
        raise SafetyError("State directory ancestors must not be symlinks")
    for ancestor in (directory, *directory.parents):
        info = ancestor.stat()
        if info.st_uid not in {0, os.geteuid()} or (
            info.st_mode & 0o022 and not info.st_mode & stat.S_ISVTX
        ):
            raise SafetyError(
                "State directory ancestors must be trusted and protected from replacement"
            )
    info = directory.stat()
    if (
        not stat.S_ISDIR(info.st_mode)
        or info.st_uid != os.geteuid()
        or info.st_mode & 0o077
    ):
        raise SafetyError(
            "State directory must already exist, be owned by this user and be private (0700)"
        )
    for target in (path, path.with_name(path.name + ".lock")):
        try:
            info = target.lstat()
        except FileNotFoundError:
            continue
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_uid != os.geteuid()
            or info.st_nlink != 1
            or info.st_mode & 0o077
            or not info.st_mode & 0o200
        ):
            raise SafetyError(
                "Existing state/lock must be private, writable, single-link regular files owned by this user"
            )
    return path


@contextlib.contextmanager
def run_lock(path):
    path = state_path(path)
    descriptor = os.open(
        str(path) + ".lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600
    )
    try:
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise SafetyError("Run is already in use") from None
        yield
    finally:
        os.close(descriptor)


def save(path, state):
    path = state_path(path)
    content = json.dumps(state, sort_keys=True, indent=2).encode()
    if len(content) > MAX_STATE:
        raise SafetyError("Run state exceeds size limit")
    descriptor, temporary = tempfile.mkstemp(dir=path.parent, prefix=".wmco-")
    try:
        with os.fdopen(descriptor, "wb") as output:
            output.write(content)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, state_path(path))
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def new_state(plan):
    return {
        "schema": 1,
        "plan": plan,
        "approval": digest(plan),
        "jobs": {
            job: {
                "phase": "not_submitted",
                "id": "",
                "status": "MISSING",
                "job_url": "",
                "completion_time": "",
                "terminal_status": "",
            }
            for job in plan["jobs"]
        },
    }


def load(path):
    path = state_path(path)
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW), "rb") as source:
        content = source.read(MAX_STATE + 1)
    if len(content) > MAX_STATE:
        raise SafetyError("Run state exceeds size limit")
    state = json.loads(content, object_pairs_hook=unique_json)
    if set(state) != {"schema", "plan", "approval", "jobs"} or state["schema"] != 1:
        raise SafetyError("Unsupported run state schema")
    plan = state["plan"]
    if set(plan) != {
        "ocp_minor",
        "stream",
        "source_sha",
        "jobs",
        "catalog_refs",
        "source_fingerprint",
    }:
        raise SafetyError("Unsupported plan schema")
    inputs(plan["ocp_minor"], plan["stream"], plan["source_sha"])
    jobs = plan["jobs"]
    if (
        not isinstance(jobs, list)
        or not 1 <= len(jobs) <= MAX_JOBS
        or jobs != sorted(set(jobs))
        or set(jobs) != set(state["jobs"])
        or state["approval"] != digest(plan)
        or not re.fullmatch(r"[0-9a-f]{64}", plan["source_fingerprint"])
        or not isinstance(plan["catalog_refs"], list)
        or not set(plan["catalog_refs"]) <= {CONNECTED, DISCONNECTED}
    ):
        raise SafetyError("Run plan identity or job set is invalid")
    identifiers = set()
    for job, row in state["jobs"].items():
        if not re.fullmatch(
            r"periodic-ci-openshift-openshift-tests-private-release-"
            + re.escape(plan["ocp_minor"])
            + r"-amd64-nightly-[a-z0-9-]+-winc(?:-[a-z0-9]+)*",
            job,
        ):
            raise SafetyError("Invalid ordinary-nightly job name in state")
        if set(row) != {
            "phase",
            "id",
            "status",
            "job_url",
            "completion_time",
            "terminal_status",
        }:
            raise SafetyError("Unsupported job state schema")
        if row["phase"] not in {
            "not_submitted",
            "submitting",
            "submitted",
            "uncertain",
        } or row["status"] not in STATES | {
            "MISSING",
            "UNCERTAIN",
            "POLL_ERROR",
            "INCOMPLETE",
            "CONFLICT",
        }:
            raise SafetyError("Unsupported job phase/status")
        if row["phase"] == "submitted":
            identifier = execution_id(row["id"])
            if identifier in identifiers:
                raise SafetyError("Duplicate execution ID")
            identifiers.add(identifier)
        elif (
            row["id"]
            or row["terminal_status"]
            or row["job_url"]
            or row["completion_time"]
        ):
            raise SafetyError("Unsubmitted job has execution evidence")
        if row["job_url"]:
            prow_url(row["job_url"], job)
        if row["completion_time"]:
            timestamp(row["completion_time"])
        if row["terminal_status"] not in TERMINAL | {""}:
            raise SafetyError("Unknown terminal evidence")
        if row["status"] == "SUCCESS" and (
            row["terminal_status"] != "SUCCESS"
            or not row["completion_time"]
            or not row["job_url"]
        ):
            raise SafetyError("Success lacks terminal evidence")
    return state


def unique_json(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise SafetyError("Duplicate JSON field")
        result[key] = value
    return result


def submit(plan, path, client):
    if path.exists() or path.is_symlink():
        raise SafetyError("State already exists; use resume to poll, never resubmit")
    state = new_state(plan)
    save(path, state)
    for job, row in state["jobs"].items():
        row["phase"], row["status"] = "submitting", "UNCERTAIN"
        save(path, state)
        try:
            result = client.request("POST", job=job)
            identifier = validate_execution(result, job)
            if result["job_status"] != "TRIGGERED" or identifier in {
                entry["id"] for entry in state["jobs"].values()
            }:
                raise SafetyError(
                    "Unexpected submission response or duplicate execution ID"
                )
            row.update(phase="submitted", id=identifier, status="TRIGGERED")
            save(path, state)
        except BaseException:
            row.update(phase="uncertain", id="", status="UNCERTAIN")
            save(path, state)
            raise
    return state


def observe(state, path, client):
    for job, row in state["jobs"].items():
        if row["phase"] == "submitting":
            row.update(phase="uncertain", status="UNCERTAIN")
            save(path, state)
        if row["phase"] != "submitted" or row["status"] == "CONFLICT":
            continue
        try:
            result = client.request("GET", identifier=row["id"])
            validate_execution(result, job, row["id"])
            status, url = result["job_status"], result.get("job_url", "")
            completion = result.get("completion_time", "")
            if row["job_url"] and url != row["job_url"]:
                raise EvidenceConflict("Missing or conflicting Prow run identity")
            if row["terminal_status"] and (
                status != row["terminal_status"]
                or (row["completion_time"] and completion != row["completion_time"])
            ):
                raise EvidenceConflict("Conflicting or stale terminal execution result")
            row["job_url"] = url
            if status in TERMINAL:
                row["terminal_status"] = status
                if completion:
                    finished = timestamp(completion)
                    if result.get("create_time") and finished < timestamp(
                        result["create_time"]
                    ):
                        raise SafetyError("Completion precedes execution start")
                    row["completion_time"] = completion
                if status == "SUCCESS" and (not completion or not url):
                    status = "INCOMPLETE"
            elif completion:
                raise SafetyError("Nonterminal execution has a completion timestamp")
            row["status"] = status
        except (SafetyError, ValueError, TypeError, KeyError) as error:
            row["status"] = (
                "CONFLICT" if isinstance(error, EvidenceConflict) else "POLL_ERROR"
            )
            message = (
                str(error)
                if isinstance(error, SafetyError)
                else "Malformed execution response"
            )
            print(f"{job}: GET: {message}; no resubmission", file=sys.stderr)
        save(path, state)


def report(state):
    success = True
    for job, row in state["jobs"].items():
        status = (
            row["status"]
            if row["phase"] == "submitted"
            else ("MISSING" if row["phase"] == "not_submitted" else "UNCERTAIN")
        )
        link = row["job_url"] or (
            f"https://prow.ci.openshift.org/prowjob?prowjob={row['id']}"
            if row["id"]
            else "no execution ID"
        )
        print(f"{job}: {status} | {link}")
        success &= (
            row["phase"] == "submitted"
            and status == "SUCCESS"
            and row["terminal_status"] == "SUCCESS"
            and bool(row["completion_time"])
            and bool(row["job_url"])
        )
    print(
        "Overall: SUCCESS"
        if success
        else "Overall: NOT SUCCESS (incomplete, uncertain, or unsuccessful jobs)"
    )
    return success


def monitor(state, path, client, wait_seconds=0, poll_seconds=30):
    deadline = time.monotonic() + wait_seconds
    while True:
        observe(state, path, client)
        if report(state):
            return 0
        if time.monotonic() >= deadline or all(
            row["status"] in TERMINAL | {"CONFLICT"} for row in state["jobs"].values()
        ):
            return 2
        time.sleep(min(poll_seconds, max(0, deadline - time.monotonic())))


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    for action in ("plan", "start", "resume"):
        command = commands.add_parser(action)
        if action != "resume":
            command.add_argument("ocp_minor")
            command.add_argument("stream")
            command.add_argument(
                "--source-sha",
                required=True,
                help="reviewed openshift/release full commit SHA",
            )
        if action != "plan":
            command.add_argument(
                "--state",
                required=True,
                help="absolute .json path in an existing owned private directory; no traversal or symlinks",
            )
            command.add_argument(
                "--token-stdin",
                required=True,
                action="store_true",
                help="read an already-approved app.ci OAuth token from a provider on stdin; never prompt",
            )
            command.add_argument("--wait-seconds", type=int, default=0)
            command.add_argument("--poll-seconds", type=int, default=30)
        if action == "start":
            command.add_argument("--approved-plan-digest", required=True)
            command.add_argument(
                "--confirm-live", required=True, choices=["TRIGGER_EXISTING_PERIODICS"]
            )
    args = parser.parse_args(argv)
    try:
        if args.action != "plan":
            args.state = state_path(args.state)
        if args.action != "resume":
            plan = build_plan(args.ocp_minor, args.stream, args.source_sha)
            if args.action == "plan":
                print(json.dumps(plan, indent=2, sort_keys=True))
                print(f"Approval digest: {digest(plan)}")
                return 0
            if args.approved_plan_digest != digest(plan):
                raise SafetyError(
                    "Plan changed or not approved; review the exact job list and digest"
                )
        if not 0 <= args.wait_seconds <= 21600 or not 1 <= args.poll_seconds <= 300:
            raise SafetyError(
                "Wait must be 0..21600 seconds and poll interval 1..300 seconds"
            )
        with run_lock(args.state):
            state = load(args.state) if args.action == "resume" else None
            token = sys.stdin.read(8194).removesuffix("\n")
            client = Gangway(token)
            if state is None:
                state = submit(plan, args.state, client)
            return monitor(
                state, args.state, client, args.wait_seconds, args.poll_seconds
            )
    except (
        SafetyError,
        OSError,
        ValueError,
        TypeError,
        KeyError,
        IndexError,
        AttributeError,
        RecursionError,
        KeyboardInterrupt,
    ) as error:
        message = (
            str(error)
            if isinstance(error, SafetyError)
            else "Invalid input/state/source shape, local I/O failure, or interrupted operation"
        )
        print(
            f"Stopped: {message}. Existing state must be inspected/resumed; never retry an uncertain POST.",
            file=sys.stderr,
        )
        return 2


if __name__ == "__main__":
    sys.exit(main())
