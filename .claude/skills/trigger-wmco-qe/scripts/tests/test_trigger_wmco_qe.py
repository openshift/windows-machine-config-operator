import contextlib
import copy
import http.client
import importlib.util
import io
import json
import os
import tempfile
import unittest
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from unittest import mock

import yaml

SPEC = importlib.util.spec_from_file_location(
    "wmco", Path(__file__).parents[1] / "trigger_wmco_qe.py"
)
wmco = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(wmco)
SHA = "b0cb7d2854bb16b43bb855433869a11420693457"
TOKEN = "offline-placeholder-token"
FINISHED = "2026-11-03T12:00:00Z"


class Source:
    def __init__(self, version="5.0", extra=()):
        self.files = {}
        self.documents = {}
        suffix = "f14" if version == "4.21" else "f7"
        topologies = ["aws-ipi", "gcp-ipi", "azure-ipi"]
        if version == "5.0":
            topologies += ["aws-upi", "vsphere-ipi-disconnected"]
        tests, periodics = [], []
        stem = f"openshift-openshift-tests-private-release-{version}"
        self.config = f"ci-operator/config/openshift/openshift-tests-private/{stem}__amd64-nightly.yaml"
        self.periodics = (
            f"ci-operator/jobs/openshift/openshift-tests-private/{stem}-periodics.yaml"
        )
        for topology in topologies + list(extra):
            name = f"{topology}-ovn-winc-{suffix}"
            workflow = f"cucushift-installer-rehearse-{topology}-ovn-winc"
            chain = workflow + "-provision"
            disconnected = "disconnected" in topology
            producer = wmco.DISCONNECTED if disconnected else wmco.CONNECTED
            consumer = (
                "openshift-windows-install-wmco"
                if disconnected
                else "operatorhub-subscribe"
            )
            self.put(
                wmco.registry_path(workflow, "workflow"),
                {
                    "workflow": {
                        "steps": {
                            "pre": [{"chain": chain}],
                            "env": {
                                "SUB_SOURCE": "wmco",
                                "SUB_PACKAGE": "windows-machine-config-operator",
                                "SUB_CHANNEL": "stable",
                            },
                        }
                    }
                },
            )
            self.put(
                wmco.registry_path(chain, "chain"),
                {"chain": {"steps": [{"ref": producer}, {"ref": consumer}]}},
            )
            path = f"{wmco.REGISTRY}/openshift/windows/{producer.removeprefix('openshift-windows-')}/{producer}-commands.sh"
            self.documents[path] = (
                "trusted upstream catalog producer source for offline fingerprinting\n"
            )
            tests.append(
                {
                    "as": name,
                    "cron": "0 0 31 2 *",
                    "steps": {
                        "cluster_profile": "fixture-only",
                        "env": {"TEST_SCENARIOS": "Windows_Containers"},
                        "workflow": workflow,
                    },
                }
            )
            if disconnected:
                tests[-1]["steps"]["leases"] = [
                    {
                        "env": "VSPHERE_BASTION_LEASED_RESOURCE",
                        "resource_type": "vsphere-connected-2-quota-slice",
                    }
                ]
            periodics.append(
                {
                    "name": f"periodic-ci-{stem}-amd64-nightly-{name}",
                    "cron": "0 0 31 2 *",
                    "labels": {"ci-operator.openshift.io/variant": "amd64-nightly"},
                    "spec": {
                        "containers": [
                            {"args": [f"--target={name}", "--variant=amd64-nightly"]}
                        ]
                    },
                }
            )
        upgrade = copy.deepcopy(periodics[0])
        upgrade["name"] = upgrade["name"].replace(
            "-amd64-nightly-", f"-amd64-nightly-{version}-upgrade-from-stable-4.20-"
        )
        upgrade["labels"]["ci-operator.openshift.io/variant"] = (
            f"amd64-nightly-{version}-upgrade-from-stable-4.20"
        )
        self.put(
            self.config,
            {
                "releases": {
                    "latest": {
                        "candidate": {
                            "architecture": "amd64",
                            "product": "ocp",
                            "stream": "nightly",
                            "version": version,
                        }
                    }
                },
                "tests": tests,
            },
        )
        self.put(self.periodics, {"periodics": periodics + [upgrade]})

    def put(self, path, value):
        self.documents[path] = yaml.safe_dump(value)

    def edit(self, path, change):
        value = yaml.safe_load(self.documents[path])
        change(value)
        self.put(path, value)

    def read(self, path):
        self.files[path] = self.documents[path]
        return self.files[path]


class Response(io.BytesIO):
    def __init__(self, url, value, status=200, raw=False):
        super().__init__(value if raw else json.dumps(value).encode())
        self.url, self.status = url, status

    def geturl(self):
        return self.url


class OfflineTest(unittest.TestCase):
    def setUp(self):
        self.network = mock.patch.object(
            urllib.request.OpenerDirector,
            "open",
            side_effect=AssertionError("Live HTTP is forbidden"),
        )
        self.network.start()
        self.addCleanup(self.network.stop)
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.path = Path(directory.name) / "run.json"
        self.source = Source()
        self.plan = wmco.build_plan("5.0", "z", SHA, self.source)
        self.client = wmco.Gangway(TOKEN)
        self.client.http = mock.Mock()
        self.posts = []
        self.requests = []
        self.results = {}
        self.client.http.open.side_effect = self.http

    def result(self, job, status="TRIGGERED", identifier=None):
        identifier = identifier or str(uuid.uuid4())
        return {
            "id": identifier,
            "job_name": job,
            "job_type": "PERIODIC",
            "job_status": status,
            "job_url": "",
        }

    def finish(self, result, status="SUCCESS"):
        result.update(
            job_status=status,
            create_time="2026-11-03T11:00:00Z",
            completion_time=FINISHED,
            job_url=f"https://prow.ci.openshift.org/view/gs/test-platform-results/logs/{result['job_name']}/123456",
        )

    def http(self, request, timeout):
        self.assertEqual(request.get_header("Authorization"), f"Bearer {TOKEN}")
        self.assertEqual(timeout, 30)
        self.requests.append(request.get_method())
        if request.get_method() == "POST":
            self.assertEqual(request.full_url, wmco.API)
            body = json.loads(request.data)
            self.assertEqual(set(body), {"job_name", "job_execution_type"})
            self.assertEqual(body["job_execution_type"], "1")
            disk = wmco.load(self.path)
            self.assertEqual(disk["jobs"][body["job_name"]]["phase"], "submitting")
            for previous in self.posts:
                self.assertEqual(
                    disk["jobs"][previous]["id"], self.results[previous]["id"]
                )
            result = self.result(body["job_name"])
            self.results[body["job_name"]] = result
            self.posts.append(body["job_name"])
        else:
            self.assertEqual(request.get_method(), "GET")
            self.assertIsNone(request.data)
            identifier = request.full_url.removeprefix(wmco.API + "/")
            result = next(
                value for value in self.results.values() if value["id"] == identifier
            )
        return Response(request.full_url, result)

    def submit(self):
        self.posts.clear()
        self.results.clear()
        with wmco.run_lock(self.path):
            return wmco.submit(self.plan, self.path, self.client)

    def observe(self, state):
        with contextlib.redirect_stderr(io.StringIO()) as errors:
            wmco.observe(state, self.path, self.client)
        return errors.getvalue()

    def run_cli(self, args, source=None):
        with (
            mock.patch.object(wmco, "PublicSource", return_value=source or Source()),
            mock.patch.object(wmco, "Gangway", return_value=self.client),
            mock.patch.object(wmco.sys, "stdin", io.StringIO(TOKEN + "\n")),
            contextlib.redirect_stdout(io.StringIO()) as output,
            contextlib.redirect_stderr(io.StringIO()) as errors,
        ):
            code = wmco.main(args)
        return code, output.getvalue(), errors.getvalue()


class DiscoveryTests(OfflineTest):
    def test_current_versions_matrix_and_upgrade_exclusion(self):
        for version, count in (("4.21", 3), ("5.0", 5)):
            for stream in ("z-stream", "y-stream"):
                with self.subTest(version=version, stream=stream):
                    plan = wmco.build_plan(version, stream, SHA, Source(version))
                    self.assertEqual(len(plan["jobs"]), count)
                    self.assertTrue(all("upgrade" not in job for job in plan["jobs"]))
                    self.assertTrue(
                        all(
                            job.endswith("f14" if version == "4.21" else "f7")
                            for job in plan["jobs"]
                        )
                    )
        self.assertTrue(
            any("vsphere-ipi-disconnected" in job for job in self.plan["jobs"])
        )
        self.assertEqual(
            self.plan["catalog_refs"], sorted([wmco.CONNECTED, wmco.DISCONNECTED])
        )

    def test_y_all_matching_platforms_and_suffix_variants(self):
        extras = ("nutanix-ipi", "aws-upi")
        source = Source(extra=extras[:1])
        source.edit(
            source.config,
            lambda value: value["tests"].append(copy.deepcopy(value["tests"][0])),
        )
        config = yaml.safe_load(source.documents[source.config])
        config["tests"][-1]["as"] += "-additional-suffix"
        source.put(source.config, config)
        periodic = yaml.safe_load(source.documents[source.periodics])
        variant = copy.deepcopy(periodic["periodics"][0])
        variant["name"] += "-additional-suffix"
        variant["spec"]["containers"][0]["args"][0] += "-additional-suffix"
        periodic["periodics"].append(variant)
        source.put(source.periodics, periodic)
        plan = wmco.build_plan("5.0", "y", SHA, source)
        self.assertEqual(len(plan["jobs"]), 7)
        self.assertTrue(any("nutanix" in job for job in plan["jobs"]))
        self.assertTrue(any(job.endswith("additional-suffix") for job in plan["jobs"]))
        z_plan = wmco.build_plan("5.0", "z", SHA, source)
        self.assertFalse(any("nutanix" in job for job in z_plan["jobs"]))
        self.assertEqual(len(z_plan["jobs"]), 6)

    def test_inputs_duplicates_unsafe_yaml_and_merges(self):
        for version, stream, sha in (
            ("10.21", "z", SHA),
            ("4.21.1", "z", SHA),
            ("04.21", "z", SHA),
            ("5.0", "x", SHA),
            ("5.0", "z", "main"),
        ):
            with (
                self.subTest(version=version, stream=stream, sha=sha),
                self.assertRaises(wmco.SafetyError),
            ):
                wmco.inputs(version, stream, sha)
        for content in (
            "key: one\nkey: two",
            "!!python/object:object {}",
            "key: &entry {}\nmerged: {<<: *entry}",
            "[]",
        ):
            with self.subTest(content=content), self.assertRaises(wmco.SafetyError):
                wmco.read_yaml(content)

    def test_explicit_safe_loader_preserves_strict_nested_mapping_rules(self):
        self.assertTrue(issubclass(wmco.UniqueLoader, yaml.SafeLoader))
        self.assertEqual(
            wmco.read_yaml("nested: {value: safe}"), {"nested": {"value": "safe"}}
        )
        with mock.patch.object(os, "system") as system:
            for content in (
                '!!python/object/apply:os.system ["must-not-run"]',
                "nested: {key: first, key: second}",
                "nested: {1: nonstring}",
                "base: &base {key: value}\nnested: {<<: *base}",
                "first: document\n---\nsecond: document",
            ):
                with self.subTest(content=content), self.assertRaises(wmco.SafetyError):
                    wmco.read_yaml(content)
            system.assert_not_called()

    def test_configuration_mismatch_fails_closed(self):
        mutations = (
            (
                "config",
                lambda value: value["releases"]["latest"]["candidate"].update(
                    version="4.99"
                ),
            ),
            ("config", lambda value: value["tests"].append(value["tests"][0])),
            (
                "config",
                lambda value: value["tests"][0]["steps"]["env"].update(
                    WMCO_INDEX_IMAGE="override"
                ),
            ),
            (
                "config",
                lambda value: value["tests"][0]["steps"]["env"].update(
                    SUB_SOURCE="override"
                ),
            ),
            ("config", lambda value: value["tests"][0].pop("cron")),
            ("config", lambda value: value["tests"][0]["steps"].update(pre=[])),
            ("periodics", lambda value: value["periodics"].pop(0)),
            (
                "periodics",
                lambda value: value["periodics"].append(value["periodics"][0]),
            ),
            (
                "periodics",
                lambda value: value["periodics"][0]["spec"]["containers"][0].update(
                    args=["--target=foreign", "--variant=amd64-nightly"]
                ),
            ),
            (
                "periodics",
                lambda value: value["periodics"][0]["labels"].update(
                    {"ci-operator.openshift.io/variant": "upgrade"}
                ),
            ),
        )
        for document, change in mutations:
            with self.subTest(document=document, change=change):
                source = Source()
                source.edit(getattr(source, document), change)
                with self.assertRaises(wmco.SafetyError):
                    wmco.build_plan("5.0", "z", SHA, source)

    def test_supported_catalog_contract_and_source_fingerprint(self):
        original = self.plan["source_fingerprint"]
        source = Source()
        script = next(path for path in source.documents if path.endswith("commands.sh"))
        source.documents[script] += "changed trusted source\n"
        self.assertNotEqual(
            wmco.build_plan("5.0", "z", SHA, source)["source_fingerprint"], original
        )
        workflow = "cucushift-installer-rehearse-aws-ipi-ovn-winc"
        for kind, change in (
            (
                "workflow",
                lambda value: value["workflow"]["steps"]["env"].update(
                    SUB_CHANNEL="other"
                ),
            ),
            ("chain", lambda value: value["chain"]["steps"].reverse()),
        ):
            source = Source()
            source.edit(
                wmco.registry_path(
                    workflow if kind == "workflow" else workflow + "-provision", kind
                ),
                change,
            )
            with self.assertRaises(wmco.SafetyError):
                wmco.build_plan("5.0", "z", SHA, source)

    def test_public_source_is_fixed_host_credential_free_and_bounded(self):
        source = wmco.PublicSource(SHA)
        source.http = mock.Mock()
        path = self.source.config
        expected_url = f"{wmco.SOURCE}/{SHA}/{path}"
        source.http.open.return_value = Response(expected_url, b"source", raw=True)
        self.assertEqual(source.read(path), "source")
        self.assertEqual(source.http.open.call_args.args, (expected_url,))
        for invalid in (
            "https://foreign/source",
            "ci-operator/../foreign",
            "ci-operator/file?token=value",
        ):
            with self.assertRaises(wmco.SafetyError):
                source.read(invalid)


class SubmissionTests(OfflineTest):
    def test_approved_start_cli_to_all_job_success(self):
        def complete_on_get(request, timeout):
            if request.get_method() == "GET":
                for result in self.results.values():
                    self.finish(result)
            return self.http(request, timeout)

        self.client.http.open.side_effect = complete_on_get
        args = [
            "start",
            "5.0",
            "z",
            "--source-sha",
            SHA,
            "--state",
            str(self.path),
            "--token-stdin",
            "--approved-plan-digest",
            wmco.digest(self.plan),
            "--confirm-live",
            "TRIGGER_EXISTING_PERIODICS",
        ]
        code, output, errors = self.run_cli(args)
        self.assertEqual(code, 0)
        self.assertIn("Overall: SUCCESS", output)
        self.assertEqual(self.requests, ["POST"] * 5 + ["GET"] * 5)
        self.assertNotIn(TOKEN, output + errors + self.path.read_text())

    def test_failed_intent_persistence_prevents_post(self):
        with (
            mock.patch.object(wmco, "save", side_effect=OSError("disk unavailable")),
            self.assertRaises(OSError),
        ):
            self.submit()
        self.client.http.open.assert_not_called()

    def test_repeated_submission_id_blocks_remaining_jobs(self):
        repeated = str(uuid.uuid4())

        def repeated_response(request, timeout):
            body = json.loads(request.data)
            return Response(
                request.full_url, self.result(body["job_name"], identifier=repeated)
            )

        self.client.http.open.side_effect = repeated_response
        with self.assertRaises(wmco.SafetyError):
            self.submit()
        rows = list(wmco.load(self.path)["jobs"].values())
        self.assertEqual(rows[0]["id"], repeated)
        self.assertEqual(rows[1]["phase"], "uncertain")
        self.assertTrue(all(row["phase"] == "not_submitted" for row in rows[2:]))
        self.assertEqual(self.client.http.open.call_count, 2)

    def test_exact_http_and_persist_before_next_submission(self):
        state = self.submit()
        self.assertEqual(len(self.posts), 5)
        self.assertEqual(len({row["id"] for row in state["jobs"].values()}), 5)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        self.assertNotIn(TOKEN, self.path.read_text())
        with self.assertRaises(wmco.SafetyError):
            self.submit()

    def test_plan_approval_is_checked_before_post(self):
        args = [
            "start",
            "5.0",
            "z",
            "--source-sha",
            SHA,
            "--state",
            str(self.path),
            "--token-stdin",
            "--approved-plan-digest",
            "incorrect",
            "--confirm-live",
            "TRIGGER_EXISTING_PERIODICS",
        ]
        code, output, errors = self.run_cli(args)
        self.assertEqual(code, 2)
        self.assertIn("not approved", errors)
        self.assertFalse(self.path.exists())
        self.assertEqual(self.requests, [])
        self.assertNotIn(TOKEN, output + errors)

    def test_uncertain_post_errors_never_repeat_or_adopt(self):
        for failure in (
            TimeoutError(TOKEN),
            http.client.IncompleteRead(TOKEN.encode()),
            urllib.error.HTTPError(wmco.API, 500, TOKEN, {}, None),
            urllib.error.HTTPError(wmco.API, 403, TOKEN, {}, None),
            KeyboardInterrupt(),
            Response(wmco.API, b"not json " + TOKEN.encode(), raw=True),
            Response(wmco.API, {"id": str(uuid.uuid4())}),
        ):
            with self.subTest(failure=type(failure).__name__):
                path = self.path.with_name(str(uuid.uuid4()) + ".json")
                if isinstance(failure, BaseException):
                    self.client.http.open.side_effect = failure
                else:
                    self.client.http.open.side_effect = None
                    self.client.http.open.return_value = failure
                with self.assertRaises((wmco.SafetyError, KeyboardInterrupt)):
                    wmco.submit(self.plan, path, self.client)
                state = wmco.load(path)
                first = next(iter(state["jobs"].values()))
                self.assertEqual((first["phase"], first["id"]), ("uncertain", ""))
                self.client.http.open.reset_mock()
                self.observe_at(state, path)
                self.client.http.open.assert_not_called()
                self.assertNotIn(TOKEN, path.read_text())

    def observe_at(self, state, path):
        with contextlib.redirect_stderr(io.StringIO()):
            wmco.observe(state, path, self.client)

    def test_second_post_failure_preserves_first_id_and_stops(self):
        real_http = self.http

        def fail_second(request, timeout):
            if len(self.posts) == 1:
                raise TimeoutError(TOKEN)
            return real_http(request, timeout)

        self.client.http.open.side_effect = fail_second
        with self.assertRaises(wmco.SafetyError):
            self.submit()
        state = wmco.load(self.path)
        rows = list(state["jobs"].values())
        self.assertEqual(rows[0]["id"], next(iter(self.results.values()))["id"])
        self.assertEqual(rows[1]["phase"], "uncertain")
        self.assertTrue(all(row["phase"] == "not_submitted" for row in rows[2:]))

    def test_crash_windows_and_resume_only_get(self):
        state = self.submit()
        rows = list(state["jobs"].values())
        rows[1].update(phase="submitting", id="", status="UNCERTAIN")
        rows[2].update(phase="not_submitted", id="", status="MISSING")
        wmco.save(self.path, state)
        self.requests.clear()
        code, output, errors = self.run_cli(
            ["resume", "--state", str(self.path), "--token-stdin"]
        )
        self.assertEqual(code, 2)
        self.assertEqual(self.requests, ["GET"] * 3)
        self.assertIn("UNCERTAIN", output)
        self.assertIn("MISSING", output)
        self.assertNotIn(TOKEN, output + errors)
        self.assertEqual(
            list(wmco.load(self.path)["jobs"].values())[1]["phase"], "uncertain"
        )

    def test_crash_after_response_before_id_save_remains_uncertain(self):
        real_save = wmco.save

        def crash(path, state):
            if any(row["id"] for row in state["jobs"].values()):
                raise KeyboardInterrupt()
            real_save(path, state)

        with (
            mock.patch.object(wmco, "save", side_effect=crash),
            self.assertRaises(KeyboardInterrupt),
        ):
            self.submit()
        state = wmco.load(self.path)
        self.assertEqual(len(self.posts), 1)
        self.client.http.open.reset_mock()
        self.observe(state)
        self.client.http.open.assert_not_called()

    def test_redirects_auth_body_and_token_validation(self):
        handler = wmco.NoRedirect()
        for url in ("http://foreign/", "https://foreign/", wmco.API):
            self.assertIsNone(handler.redirect_request(None, None, 302, "", {}, url))
        job = self.plan["jobs"][0]
        for value in ("", "contains space", "bad\nheader", "x" * 8193):
            with self.assertRaises(wmco.SafetyError):
                wmco.Gangway(value)
        for response in (
            Response("https://foreign/", {}),
            Response(wmco.API, b'{"id":"one","id":"two"}', raw=True),
            Response(wmco.API, b"x" * (wmco.MAX_STATE + 1), raw=True),
        ):
            self.client.http.open.side_effect = None
            self.client.http.open.return_value = response
            with self.assertRaises(wmco.SafetyError):
                self.client.request("POST", job=job)
        with self.assertRaises(wmco.SafetyError):
            self.client.request("GET", identifier="https://foreign/")


class StatePathTests(OfflineTest):
    def rejected_by_cli(self, path):
        for action in ("start", "resume"):
            args = [action]
            if action == "start":
                args += [
                    "5.0",
                    "z",
                    "--source-sha",
                    SHA,
                    "--approved-plan-digest",
                    wmco.digest(self.plan),
                    "--confirm-live",
                    "TRIGGER_EXISTING_PERIODICS",
                ]
            args += ["--state", str(path), "--token-stdin"]
            code, output, errors = self.run_cli(args)
            self.assertEqual(code, 2, str(path))
            self.assertEqual(self.requests, [])
            self.client.http.open.assert_not_called()
            self.assertNotIn(TOKEN, output + errors)

    def test_relative_traversal_missing_directory_and_names_rejected_before_post(self):
        root = self.path.parent
        for path in (
            "run.json",
            root / ".." / root.name / "run.json",
            root / "missing" / "run.json",
            root / "run.txt",
            root / "-run.json",
            root / ("x" * 97 + ".json"),
        ):
            with self.subTest(path=path):
                self.rejected_by_cli(path)
        self.assertFalse((root / "missing").exists())

    def test_symlink_file_lock_and_directory_escapes_rejected(self):
        root = self.path.parent
        private = root / "private"
        private.mkdir(mode=0o700)
        alias = root / "alias"
        alias.symlink_to(private, target_is_directory=True)
        self.rejected_by_cli(alias / "run.json")
        target = private / "target.json"
        target.write_text("preserve")
        target.chmod(0o600)
        self.path.symlink_to(target)
        self.rejected_by_cli(self.path)
        self.path.unlink()
        lock = self.path.with_name(self.path.name + ".lock")
        lock.symlink_to(target)
        self.rejected_by_cli(self.path)
        self.assertEqual(target.read_text(), "preserve")
        self.assertFalse((private / "run.json").exists())

    def test_shared_wrong_owner_hardlink_and_nonregular_targets_rejected(self):
        root = self.path.parent
        shared = root / "shared"
        shared.mkdir(mode=0o755)
        self.rejected_by_cli(shared / "run.json")
        shared.chmod(0o777)
        private = shared / "private"
        private.mkdir(mode=0o700)
        self.rejected_by_cli(private / "run.json")
        with mock.patch.object(wmco.os, "geteuid", return_value=os.geteuid() + 1):
            self.rejected_by_cli(self.path)
        target = root / "original.json"
        target.write_text("preserve")
        target.chmod(0o600)
        self.path.hardlink_to(target)
        self.rejected_by_cli(self.path)
        self.path.unlink()
        self.path.mkdir(mode=0o700)
        self.rejected_by_cli(self.path)
        self.path.rmdir()
        self.path.write_text("preserve")
        for mode in (0o644, 0o400):
            self.path.chmod(mode)
            self.rejected_by_cli(self.path)
        self.assertEqual(target.read_text(), "preserve")

    def test_unrelated_private_json_is_never_overwritten(self):
        original = '{"unrelated": "preserve"}'
        self.path.write_text(original)
        self.path.chmod(0o600)
        self.rejected_by_cli(self.path)
        self.assertEqual(self.path.read_text(), original)

    def test_rechecking_path_before_intent_blocks_post(self):
        real_save = wmco.save

        def change_permissions(path, state):
            real_save(path, state)
            if all(row["phase"] == "not_submitted" for row in state["jobs"].values()):
                path.parent.chmod(0o755)

        try:
            with (
                mock.patch.object(wmco, "save", side_effect=change_permissions),
                self.assertRaises(wmco.SafetyError),
            ):
                self.submit()
            self.assertEqual(self.requests, [])
        finally:
            self.path.parent.chmod(0o700)
        state = wmco.load(self.path)
        self.assertTrue(
            all(row["phase"] == "not_submitted" for row in state["jobs"].values())
        )
        code, output, errors = self.run_cli(
            ["resume", "--state", str(self.path), "--token-stdin"]
        )
        self.assertEqual(code, 2)
        self.assertNotIn(TOKEN, output + errors)
        self.assertEqual(self.requests, [])


class MonitoringTests(OfflineTest):
    def prepare_resume(self):
        state = wmco.new_state(self.plan)
        for job, row in state["jobs"].items():
            result = self.result(job, "PENDING")
            self.results[job] = result
            row.update(phase="submitted", id=result["id"], status="PENDING")
        wmco.save(self.path, state)
        return list(self.results.values())

    def resume(self, *options):
        return self.run_cli(
            ["resume", "--state", str(self.path), "--token-stdin", *options]
        )

    def test_resume_retains_incomplete_terminal_pins_and_blocks_changes(self):
        for status in ("ERROR", "FAILURE", "ABORTED", "SUCCESS"):
            with self.subTest(status=status):
                self.results.clear()
                results = self.prepare_resume()
                first = results[0]
                self.finish(first, status)
                first.pop("completion_time")
                if status == "SUCCESS":
                    first["job_url"] = first["job_url"].removesuffix("123456") + "123"
                else:
                    first["job_url"] = ""
                self.assertEqual(self.resume()[0], 2)
                row = next(iter(wmco.load(self.path)["jobs"].values()))
                self.assertEqual(row["terminal_status"], status)
                self.assertEqual(row["job_url"], first["job_url"])
                for result in results:
                    self.finish(result)
                if status == "SUCCESS":
                    first["job_url"] = first["job_url"].removesuffix("123456") + "456"
                code, output, errors = self.resume()
                self.assertEqual(code, 2)
                self.assertNotIn("Overall: SUCCESS", output)
                self.assertIn("conflict", errors.lower())
                self.finish(first)
                self.assertEqual(self.resume()[0], 2)
                row = next(iter(wmco.load(self.path)["jobs"].values()))
                self.assertEqual(row["status"], "CONFLICT")
        self.assertTrue(self.requests)
        self.assertEqual(set(self.requests), {"GET"})

    def test_resume_completes_consistent_partial_pins(self):
        for missing in ("job_url", "completion_time"):
            with self.subTest(missing=missing):
                self.results.clear()
                results = self.prepare_resume()
                for result in results:
                    self.finish(result)
                results[0].pop(missing)
                self.assertEqual(self.resume()[0], 2)
                row = next(iter(wmco.load(self.path)["jobs"].values()))
                self.assertEqual(row["terminal_status"], "SUCCESS")
                retained = "completion_time" if missing == "job_url" else "job_url"
                self.assertEqual(row[retained], results[0][retained])
                self.finish(results[0])
                self.assertEqual(self.resume()[0], 0)
        self.assertEqual(set(self.requests), {"GET"})

    def test_resume_terminal_conflict_stays_blocked_across_wait_and_reload(self):
        for wait in (False, True):
            with self.subTest(wait=wait):
                self.results.clear()
                results = self.prepare_resume()
                self.finish(results[0])
                self.assertEqual(self.resume()[0], 2)
                self.finish(results[0], "FAILURE")

                def next_cycle(seconds, results=results):
                    for result in results:
                        self.finish(result)

                with mock.patch.object(
                    wmco.time, "sleep", side_effect=next_cycle
                ) as sleep:
                    code, output, errors = self.resume(
                        *(
                            ["--wait-seconds", "10", "--poll-seconds", "1"]
                            if wait
                            else []
                        )
                    )
                self.assertEqual(code, 2)
                self.assertIn("Conflicting", errors)
                self.assertNotIn("Overall: SUCCESS", output)
                self.assertEqual(sleep.call_count, int(wait))
                for result in results:
                    self.finish(result)
                self.assertEqual(self.resume()[0], 2)
                row = next(iter(wmco.load(self.path)["jobs"].values()))
                self.assertEqual(row["status"], "CONFLICT")
        self.assertEqual(set(self.requests), {"GET"})

    def test_resume_http_error_recovers_get_only(self):
        results = self.prepare_resume()
        for result in results:
            self.finish(result)
        self.client.http.open.side_effect = urllib.error.HTTPError(
            wmco.API, 503, TOKEN, {}, None
        )
        code, output, errors = self.resume()
        self.assertEqual(code, 2)
        self.assertNotIn(TOKEN, output + errors + self.path.read_text())
        self.assertEqual(
            {row["status"] for row in wmco.load(self.path)["jobs"].values()},
            {"POLL_ERROR"},
        )
        self.client.http.open.side_effect = self.http
        code, output, errors = self.resume()
        self.assertEqual(code, 0)
        self.assertIn("Overall: SUCCESS", output)
        self.assertEqual(self.requests, ["GET"] * len(results))

    def test_resume_persists_uncertain_without_accepted_ids(self):
        state = wmco.new_state(self.plan)
        for row in state["jobs"].values():
            row.update(phase="submitting", status="UNCERTAIN")
        wmco.save(self.path, state)
        self.assertEqual(self.resume()[0], 2)
        self.assertEqual(
            {row["phase"] for row in wmco.load(self.path)["jobs"].values()},
            {"uncertain"},
        )
        self.assertEqual(self.requests, [])

    def test_all_requested_jobs_need_terminal_success_and_urls(self):
        state = self.submit()
        for result in list(self.results.values())[:-1]:
            self.finish(result)
        self.observe(state)
        with contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertFalse(wmco.report(state))
        self.assertIn("Overall: NOT SUCCESS", output.getvalue())
        self.finish(list(self.results.values())[-1])
        self.observe(state)
        with contextlib.redirect_stdout(io.StringIO()) as output:
            self.assertTrue(wmco.report(state))
        self.assertIn("Overall: SUCCESS", output.getvalue())
        self.assertEqual(output.getvalue().count("/view/gs/"), 5)
        self.assertEqual(wmco.load(self.path), state)
        self.assertEqual(self.requests.count("POST"), 5)

    def test_every_status_is_reported_without_false_success(self):
        for status in sorted(wmco.STATES):
            with self.subTest(status=status):
                self.path = self.path.with_name(str(uuid.uuid4()) + ".json")
                state = self.submit()
                for result in self.results.values():
                    if status in wmco.TERMINAL:
                        self.finish(result, status)
                    else:
                        result["job_status"] = status
                self.observe(state)
                self.assertEqual(
                    {row["status"] for row in state["jobs"].values()}, {status}
                )
                with contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(wmco.report(state), status == "SUCCESS")

    def test_foreign_missing_conflicting_and_incomplete_evidence(self):
        changes = (
            lambda result: result.update(id=str(uuid.uuid4())),
            lambda result: result.update(job_name="foreign"),
            lambda result: result.update(job_type="PRESUBMIT"),
            lambda result: result.update(job_status="JOB_EXECUTION_STATUS_UNSPECIFIED"),
            lambda result: result.pop("job_status"),
            lambda result: result.update(job_status="SUCCESS"),
            lambda result: result.update(
                job_status="SUCCESS", completion_time=FINISHED
            ),
            lambda result: result.update(
                job_status="PENDING", completion_time=FINISHED
            ),
            lambda result: result.update(job_url="https://foreign/"),
            lambda result: result.update(
                job_url="https://prow.ci.openshift.org/view/gs/test-platform-results/logs/foreign/123"
            ),
        )
        for change in changes:
            with self.subTest(change=change):
                self.path = self.path.with_name(str(uuid.uuid4()) + ".json")
                state = self.submit()
                first_job = self.plan["jobs"][0]
                result = self.results[first_job]
                original_id = result["id"]
                change(result)
                self.client.http.open.side_effect = (
                    lambda request, timeout, value=result: Response(
                        request.full_url, value
                    )
                )
                self.observe(state)
                with contextlib.redirect_stdout(io.StringIO()):
                    self.assertFalse(wmco.report(state))
                self.assertEqual(state["jobs"][first_job]["id"], original_id)
                self.client.http.open.side_effect = self.http

    def test_terminal_conflict_and_poll_failures_cannot_reuse_success(self):
        state = self.submit()
        for result in self.results.values():
            self.finish(result)
        self.observe(state)
        for failure in (
            TimeoutError(TOKEN),
            urllib.error.HTTPError(wmco.API, 401, TOKEN, {}, None),
            urllib.error.HTTPError(wmco.API, 404, TOKEN, {}, None),
            urllib.error.HTTPError(wmco.API, 429, TOKEN, {}, None),
            urllib.error.HTTPError(wmco.API, 503, TOKEN, {}, None),
        ):
            self.client.http.open.side_effect = failure
            errors = self.observe(state)
            self.assertNotIn(TOKEN, errors + self.path.read_text())
            self.assertEqual(
                {row["status"] for row in state["jobs"].values()}, {"POLL_ERROR"}
            )
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertFalse(wmco.report(state))
        self.client.http.open.side_effect = self.http
        result = next(iter(self.results.values()))
        result["job_status"] = "FAILURE"
        self.assertIn("Conflicting", self.observe(state))
        result["job_status"] = "SUCCESS"
        result["completion_time"] = "2026-11-03T12:01:00Z"
        self.observe(state)
        self.assertEqual(next(iter(state["jobs"].values()))["status"], "CONFLICT")
        self.assertEqual(self.requests.count("POST"), 5)

    def test_completion_timestamp_order_and_url_immutability(self):
        state = self.submit()
        result = next(iter(self.results.values()))
        self.finish(result)
        result["completion_time"] = "2026-11-03T10:00:00Z"
        self.assertIn("precedes", self.observe(state))
        result["completion_time"] = FINISHED
        self.observe(state)
        result["job_url"] = result["job_url"].removesuffix("123456") + "999999"
        self.assertIn("conflicting Prow", self.observe(state))

    def test_bounded_state_schema_symlink_and_exclusive_lock(self):
        state = self.submit()
        mutations = (
            lambda value: value.update(schema=2),
            lambda value: value.update(approval="foreign"),
            lambda value: value["jobs"].pop(next(iter(value["jobs"]))),
            lambda value: next(iter(value["jobs"].values())).update(phase="unknown"),
            lambda value: next(iter(value["jobs"].values())).update(status="SUCCESS"),
        )
        for change in mutations:
            corrupt = copy.deepcopy(state)
            change(corrupt)
            wmco.save(self.path, corrupt)
            with self.assertRaises(wmco.SafetyError):
                wmco.load(self.path)
        self.path.write_bytes(b"x" * (wmco.MAX_STATE + 1))
        with self.assertRaises(wmco.SafetyError):
            wmco.load(self.path)
        with (
            wmco.run_lock(self.path),
            self.assertRaises(wmco.SafetyError),
            wmco.run_lock(self.path),
        ):
            self.fail("Lock must be exclusive")
        link = self.path.with_name("symlink.json")
        link.symlink_to(self.path)
        with self.assertRaises(wmco.SafetyError):
            wmco.load(link)


if __name__ == "__main__":
    unittest.main()
